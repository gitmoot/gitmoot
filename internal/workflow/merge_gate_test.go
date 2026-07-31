package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
)

func insertIndependentMergeGateReview(t *testing.T, store *db.Store, reviewJob db.Job, reviewPayload JobPayload) {
	t.Helper()
	implementingAgent := "implementer"
	if strings.TrimSpace(reviewJob.Agent) == implementingAgent {
		implementingAgent = "different-implementer"
	}
	implementPayload := reviewPayload
	implementPayload.ReviewRound = ""
	implementPayload.Result = &AgentResult{Decision: "implemented", Summary: "implemented"}
	insertCompletedJob(t, store, db.Job{
		ID:    reviewJob.ID + "-implement-author",
		Agent: implementingAgent,
		Type:  "implement",
	}, implementPayload)
	insertCompletedJob(t, store, reviewJob, reviewPayload)
}

type mergeGateReviewFixture struct {
	id        string
	agent     string
	state     JobState
	headSHA   string
	decision  string
	hasResult bool
	recorded  string
}

func newMergeGateQuorumScenario(t *testing.T) (*db.Store, *fakeMergeGateGitHub, PolicyMergeGate, MergeRequest) {
	t.Helper()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "implementer", Type: "implement"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		TaskID:      "task-9",
		Result:      &AgentResult{Decision: "implemented", Summary: "implemented"},
	})
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}
	request := MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"}
	return store, gh, gate, request
}

func insertMergeGateReviewFixture(t *testing.T, store *db.Store, fixture mergeGateReviewFixture) {
	t.Helper()
	state := fixture.state
	if state == "" {
		state = JobSucceeded
	}
	headSHA := fixture.headSHA
	if headSHA == "" {
		headSHA = "head123"
	}
	payload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     headSHA,
		TaskID:      "task-9",
		ReviewRound: "review-1",
	}
	if fixture.hasResult {
		payload.Result = &AgentResult{Decision: fixture.decision, Summary: "fixture verdict"}
	}
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID:      fixture.id,
		Agent:   fixture.agent,
		Type:    "review",
		State:   string(state),
		Payload: encoded,
	}, db.JobEvent{Kind: string(state), Message: "fixture state"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if fixture.recorded != "" {
		setMergeGateJobTimestamps(t, store, fixture.id, fixture.recorded)
	}
}

func insertMergeGateDelegationChild(t *testing.T, store *db.Store, parentID, delegationID string, state JobState, result *AgentResult) {
	t.Helper()
	payload, err := marshalPayload(JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		TaskID:      "task-9",
		Result:      result,
	})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID:           parentID + "/delegation/" + delegationID,
		Agent:        delegationID,
		Type:         "review",
		State:        string(state),
		Payload:      payload,
		ParentJobID:  parentID,
		DelegationID: delegationID,
	}, db.JobEvent{Kind: string(state), Message: "delegation fixture state"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
}

func insertMergeGateHeadlessIntegrationParent(t *testing.T, store *db.Store, parentID string) {
	t.Helper()
	insertCompletedJob(t, store, db.Job{
		ID:           parentID,
		Agent:        "reviewer-a",
		Type:         "review",
		DelegationID: "verify-parent",
	}, JobPayload{
		Repo:         "gitmoot/gitmoot",
		PullRequest:  9,
		TaskID:       "task-9",
		ReviewRound:  "review-1",
		DelegationID: "verify-parent",
		WorktreePath: "/tmp/gitmoot/integration-verify-parent",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "integration review synthesized delegated evidence",
		},
	})
}

func TestPolicyMergeGateMergesPassingPullRequest(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if acquired, err := store.AcquireLock(ctx, db.BranchLock{RepoFullName: "gitmoot/gitmoot", Branch: "task-9", Owner: "lead"}); err != nil || !acquired {
		t.Fatalf("AcquireLock returned acquired=%v err=%v", acquired, err)
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "malformed-unrelated-implement", Agent: "other", Type: "implement",
		State: string(JobSucceeded), Payload: "{not-json",
	}, db.JobEvent{Kind: string(JobSucceeded), Message: "malformed unrelated fixture"}); err != nil {
		t.Fatalf("CreateJobWithEvent malformed unrelated implement: %v", err)
	}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: git, CheckoutPath: t.TempDir(), DeleteBranch: true}

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
	if gh.prCheckCalls != 0 || len(gh.checkRefs) != 1 || gh.checkRefs[0] != "head123" {
		t.Fatalf("check calls = pr:%d refs:%v; want only exact-head check-runs", gh.prCheckCalls, gh.checkRefs)
	}
	// A PR with a passing external check merges through the gate WITHOUT the
	// synthetic gitmoot/ci no-CI stamp (#596: that stamp is only for genuinely
	// CI-less heads, and only after the grace window).
	if !hasStatus(gh.statuses, gitmootMergeGateContext, "success") || hasStatus(gh.statuses, gitmootNoCIContext, "success") {
		t.Fatalf("statuses = %+v", gh.statuses)
	}
	if len(gh.statuses) != 1 || gh.statuses[0].SHA != "head123" {
		t.Fatalf("success status inputs = %+v, want head SHA head123 after branch deletion", gh.statuses)
	}
	if got := strings.Join(gh.operations, ","); got != "merge,status:gitmoot/merge-gate:success" {
		t.Fatalf("GitHub write order = %q, want merge before success status", got)
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

func TestPolicyMergeGateMergeFailureDoesNotPostSuccess(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "ready"},
	})
	mergeable := true
	mergeErr := errors.New("draft pull request cannot be merged")
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number:    9,
			State:     "open",
			HeadRef:   "task-9",
			BaseRef:   "main",
			HeadSHA:   "head123",
			Mergeable: &mergeable,
		},
		status: github.CombinedStatus{
			State:    "success",
			Statuses: []github.CommitStatus{{Context: "ci", State: "success"}},
		},
		checks:   []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeErr: mergeErr,
	}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	_, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if !errors.Is(err, mergeErr) {
		t.Fatalf("Evaluate error = %v, want %v", err, mergeErr)
	}
	if hasStatus(gh.statuses, gitmootMergeGateContext, "success") {
		t.Fatalf("statuses after failed merge = %+v, must not contain merge-gate success", gh.statuses)
	}
}

func TestPolicyMergeGateStatusFailureAfterMergeIsBestEffort(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
			HeadRef:   "task-9",
			BaseRef:   "main",
			HeadSHA:   "head123",
			Mergeable: &mergeable,
		},
		status: github.CombinedStatus{
			State:    "success",
			Statuses: []github.CommitStatus{{Context: "ci", State: "success"}},
		},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
		statusErr:   errors.New("status API unavailable"),
	}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error after completed merge: %v", err)
	}
	if !decision.Merged || decision.MergeCommitSHA != "merge123" {
		t.Fatalf("decision = %+v, want completed merge despite status error", decision)
	}
	if got := strings.Join(gh.operations, ","); got != "merge,status:gitmoot/merge-gate:success" {
		t.Fatalf("GitHub write order = %q, want merge before best-effort success status", got)
	}
}

func TestPolicyMergeGateRejectsUnverifiableReviewAuthorship(t *testing.T) {
	for _, tc := range []struct {
		name              string
		implementingAgent string
		reviewerAgent     string
		wantReason        string
	}{
		{
			name:              "self approval",
			implementingAgent: "sol",
			reviewerAgent:     "sol",
			wantReason:        "approval was authored by sol, the implementing agent; an independent reviewer is required",
		},
		{
			name:              "unattributed reviewer",
			implementingAgent: "sol",
			reviewerAgent:     "",
			wantReason:        "approval has no recorded reviewer author; an independent reviewer cannot be verified",
		},
		{
			name:          "unknown implementer",
			reviewerAgent: "audit",
			wantReason:    "approval cannot be verified as independent because the implementing agent for this task could not be determined",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			payload := JobPayload{
				Repo:        "gitmoot/gitmoot",
				Branch:      "task-9",
				PullRequest: 9,
				HeadSHA:     "head123",
				TaskID:      "task-9",
				ReviewRound: "review-1",
			}
			if tc.implementingAgent != "" {
				implementPayload := payload
				implementPayload.ReviewRound = ""
				implementPayload.Result = &AgentResult{Decision: "implemented", Summary: "implemented"}
				insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: tc.implementingAgent, Type: "implement"}, implementPayload)
			}
			reviewPayload := payload
			reviewPayload.Result = &AgentResult{Decision: "approved", Summary: "approved"}
			insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: tc.reviewerAgent, Type: "review"}, reviewPayload)

			mergeable := true
			gh := &fakeMergeGateGitHub{
				pr: github.PullRequest{
					Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
					HeadSHA: "head123", Mergeable: &mergeable,
				},
				status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
				checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
				mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
			}
			gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

			decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Ready || decision.Merged {
				t.Fatalf("decision = %+v, want escalating LeaveOpen", decision)
			}
			if !strings.Contains(decision.Reason, tc.wantReason) {
				t.Fatalf("decision reason = %q, want %q", decision.Reason, tc.wantReason)
			}
			if len(gh.merges) != 0 {
				t.Fatalf("unverifiable approval issued merge: %+v", gh.merges)
			}
		})
	}
}

func TestPolicyMergeGatePreservesSelfApprovalReasonWhenHeadMismatchSortsFirst(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	basePayload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		TaskID:      "task-9",
	}
	implementPayload := basePayload
	implementPayload.Result = &AgentResult{Decision: "implemented", Summary: "implemented"}
	insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "sol", Type: "implement"}, implementPayload)

	selfReview := basePayload
	selfReview.HeadSHA = "head123"
	selfReview.ReviewRound = "review-1"
	selfReview.Result = &AgentResult{Decision: "approved", Summary: "self-approved"}
	insertCompletedJob(t, store, db.Job{ID: "review-z-self", Agent: "sol", Type: "review"}, selfReview)

	staleReview := basePayload
	staleReview.HeadSHA = "old123"
	staleReview.ReviewRound = "review-1"
	staleReview.Result = &AgentResult{Decision: "approved", Summary: "stale approval"}
	insertCompletedJob(t, store, db.Job{ID: "review-a-stale", Agent: "audit", Type: "review"}, staleReview)

	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status: github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks: []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
	}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Ready || decision.Merged {
		t.Fatalf("decision = %+v, want escalating LeaveOpen", decision)
	}
	if !strings.Contains(decision.Reason, "approval was authored by sol, the implementing agent") {
		t.Fatalf("decision reason lost self-approval cause: %q", decision.Reason)
	}
	if strings.Contains(decision.Reason, "different head SHA") {
		t.Fatalf("incidental stale-head error replaced self-approval cause: %q", decision.Reason)
	}
}

func TestPolicyMergeGatePreservesSelfApprovalReasonWhenSelfApprovalSortsFirst(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	basePayload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		TaskID:      "task-9",
	}
	implementPayload := basePayload
	implementPayload.Result = &AgentResult{Decision: "implemented", Summary: "implemented"}
	insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "sol", Type: "implement"}, implementPayload)

	selfReview := basePayload
	selfReview.HeadSHA = "head123"
	selfReview.ReviewRound = "review-1"
	selfReview.Result = &AgentResult{Decision: "approved", Summary: "self-approved"}
	insertCompletedJob(t, store, db.Job{ID: "review-a-self", Agent: "sol", Type: "review"}, selfReview)

	staleReview := basePayload
	staleReview.HeadSHA = "old123"
	staleReview.ReviewRound = "review-1"
	staleReview.Result = &AgentResult{Decision: "approved", Summary: "stale approval"}
	insertCompletedJob(t, store, db.Job{ID: "review-z-stale", Agent: "audit", Type: "review"}, staleReview)

	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status: github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks: []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
	}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Ready || decision.Merged {
		t.Fatalf("decision = %+v, want escalating LeaveOpen", decision)
	}
	if !strings.Contains(decision.Reason, "approval was authored by sol, the implementing agent") {
		t.Fatalf("decision reason lost self-approval cause: %q", decision.Reason)
	}
	if strings.Contains(decision.Reason, "different head SHA") {
		t.Fatalf("incidental stale-head error replaced self-approval cause: %q", decision.Reason)
	}
}

func TestPolicyMergeGateExplicitKillSwitchLeavesOpenWithoutGitHubCalls(t *testing.T) {
	ctx := context.Background()
	gh := &fakeMergeGateGitHub{}
	gate := PolicyMergeGate{GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	for attempt := 0; attempt < 2; attempt++ {
		decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "owner/repo", PullRequest: 17})
		if err != nil {
			t.Fatalf("Evaluate attempt %d: %v", attempt+1, err)
		}
		if !decision.LeaveOpen || decision.Ready || decision.Merged || decision.Deferred || decision.Reason != MergeLeaveOpenAutoMergeKillSwitchReason {
			t.Fatalf("decision attempt %d = %+v", attempt+1, decision)
		}
	}
	if gh.getCalls != 0 || gh.statusCalls != 0 || gh.compareCalls != 0 || gh.checkCalls != 0 || len(gh.statuses) != 0 || len(gh.merges) != 0 {
		t.Fatalf("explicit auto_merge=false touched GitHub: %+v", gh)
	}
}

func TestRunMergeGateExplicitKillSwitchParksReviewedAndUnreviewedTasks(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	gh := &fakeMergeGateGitHub{}
	engine := Engine{Store: store, MergeGate: PolicyMergeGate{GitHub: gh}}

	for _, tc := range []struct {
		name       string
		taskID     string
		withReview bool
	}{
		{name: "no reviewers", taskID: "task-unreviewed"},
		{name: "approved review", taskID: "task-reviewed", withReview: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.UpsertTask(ctx, db.Task{ID: tc.taskID, RepoFullName: "owner/repo", Title: tc.name, State: string(TaskReadyToMerge), Branch: tc.taskID}); err != nil {
				t.Fatalf("UpsertTask: %v", err)
			}
			payload := JobPayload{Repo: "owner/repo", Branch: tc.taskID, PullRequest: 17, TaskID: tc.taskID, TaskTitle: tc.name}
			if tc.withReview {
				insertIndependentMergeGateReview(t, store, db.Job{ID: "review-" + tc.taskID, Agent: "reviewer", Type: "review"}, JobPayload{
					Repo: "owner/repo", Branch: tc.taskID, PullRequest: 17, TaskID: tc.taskID,
					Result: &AgentResult{Decision: "approved", Summary: "approved"},
				})
			}
			for attempt := 0; attempt < 2; attempt++ {
				decision, err := engine.runMergeGate(ctx, "", payload, taskRef{ID: tc.taskID, Repo: "owner/repo", Title: tc.name, Branch: tc.taskID})
				if err != nil {
					t.Fatalf("runMergeGate attempt %d: %v", attempt+1, err)
				}
				if !decision.LeaveOpen || decision.Reason != MergeLeaveOpenAutoMergeKillSwitchReason {
					t.Fatalf("decision attempt %d = %+v", attempt+1, decision)
				}
			}
			task, err := store.GetTask(ctx, tc.taskID)
			if err != nil || task.State != string(TaskAwaitingHumanMerge) {
				t.Fatalf("task = %+v, err=%v; want awaiting_human_merge", task, err)
			}
			events, err := store.ListTaskEvents(ctx, tc.taskID)
			if err != nil || len(events) != 1 || events[0].Kind != "task_awaiting_human_merge" || events[0].Reason != MergeLeaveOpenAutoMergeKillSwitchReason {
				t.Fatalf("events = %+v, err=%v", events, err)
			}
		})
	}
	if gh.getCalls != 0 || gh.statusCalls != 0 || gh.compareCalls != 0 || gh.checkCalls != 0 || len(gh.statuses) != 0 || len(gh.merges) != 0 {
		t.Fatalf("kill-switch task gate touched GitHub across repeated evaluations: %+v", gh)
	}
}

func TestPolicyMergeGateHumanRequestRequiresFinalReview(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{
		AutoMerge: false,
		Store:     store,
		GitHub:    gh,
		Git:       &fakeMergeGateGit{clean: true},
	}

	decision, err := gate.Evaluate(ctx, MergeRequest{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
		HumanMergeRequested: true,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want escalating LeaveOpen", decision)
	}
	if !strings.Contains(decision.Reason, "final agent review is not captured") {
		t.Fatalf("decision reason = %q, want missing final review", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateHumanRequestRequiresPassingCI(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success"},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "fail", State: "COMPLETED"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{
		AutoMerge: false,
		Store:     store,
		GitHub:    gh,
		Git:       &fakeMergeGateGit{clean: true},
	}

	decision, err := gate.Evaluate(ctx, MergeRequest{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
		HumanMergeRequested: true,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want escalating LeaveOpen", decision)
	}
	if !strings.Contains(decision.Reason, "external CI") {
		t.Fatalf("decision reason = %q, want external CI failure", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateHumanRequestBypassesOnlyAutoMergePolicy(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{
		AutoMerge: false,
		Store:     store,
		GitHub:    gh,
		Git:       &fakeMergeGateGit{clean: true},
	}

	decision, err := gate.Evaluate(ctx, MergeRequest{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
		HumanMergeRequested: true,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !decision.Merged || decision.LeaveOpen || decision.EscalateMergeGateMiss {
		t.Fatalf("decision = %+v", decision)
	}
	if gh.statusCalls != 1 || gh.checkCalls != 1 {
		t.Fatalf("human request evidence calls: status=%d checks=%d, want 1 each", gh.statusCalls, gh.checkCalls)
	}
	if len(gh.merges) != 1 || gh.merges[0].MatchHeadCommit != "head123" {
		t.Fatalf("merge calls = %+v", gh.merges)
	}
}

func TestPolicyMergeGateJournalFailureDoesNotChangeMergedDecision(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "native-journal-link", Agent: "audit", Type: "review"}, JobPayload{
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

	gate := PolicyMergeGate{AutoMerge: true, Store: store}
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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, Worktrees: cleaner}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, Worktrees: cleaner}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}, Worktrees: cleaner}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: git, CheckoutPath: checkout}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: git, CheckoutPath: checkout}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "head123", TaskID: "task-9",
		ReviewRound: "review-1", Result: &AgentResult{Decision: "approved"},
	})
	gh := &fakeMergeGateGitHub{pr: github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123"}}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: false}}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready {
		t.Fatalf("decision = %+v", decision)
	}
	if !decision.LeaveOpen || !strings.Contains(decision.Reason, "not successful") {
		t.Fatalf("decision = %+v, want informative leave-open gate miss", decision)
	}
}

func TestPolicyMergeGateAllowsSkippedExternalCI(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

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

func TestPolicyMergeGateReviewOptionalDoesNotBypassMandatoryReview(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr:          github.PullRequest{Number: 9, HeadRef: "task-9", BaseRef: "main", HeadSHA: "head123", Mergeable: &mergeable},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", ReviewOptional: true})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || !strings.Contains(decision.Reason, "final agent review is not captured") {
		t.Fatalf("decision = %+v, want mandatory review gate miss", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v, want none", gh.merges)
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: false}}

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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-nine", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "old123",
		TaskID:      "task-9",
		ReviewRound: "review-9",
		Result:      &AgentResult{Decision: "changes_requested", Summary: "old change"},
	})
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-ten", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestPolicyMergeGateBlocksAnyVerdictAtEvaluatedHeadBeforeRoundSelection(t *testing.T) {
	for _, tc := range []struct {
		name          string
		approvalRound string
	}{
		{
			name: "empty rounds do not use job ID to mask objection",
		},
		{
			name:          "numbered round does not mask unnumbered objection",
			approvalRound: "review-2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			basePayload := JobPayload{
				Repo:        "gitmoot/gitmoot",
				PullRequest: 9,
				HeadSHA:     "head123",
				TaskID:      "task-9",
			}
			implementPayload := basePayload
			implementPayload.HeadSHA = ""
			implementPayload.Result = &AgentResult{Decision: "implemented", Summary: "implemented"}
			insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "implementer", Type: "implement"}, implementPayload)

			objection := basePayload
			objection.Result = &AgentResult{Decision: "changes_requested", Summary: "must fix"}
			insertCompletedJob(t, store, db.Job{ID: "review-a-objection", Agent: "objector", Type: "review"}, objection)

			approval := basePayload
			approval.ReviewRound = tc.approvalRound
			approval.Result = &AgentResult{Decision: "approved", Summary: "ready"}
			insertCompletedJob(t, store, db.Job{ID: "review-z-approval", Agent: "approver", Type: "review"}, approval)

			mergeable := true
			gh := &fakeMergeGateGitHub{
				pr: github.PullRequest{
					Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
					HeadSHA: "head123", Mergeable: &mergeable,
				},
				status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
				checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
				mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
			}
			gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

			decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
				t.Fatalf("decision = %+v, want objection to block merge", decision)
			}
			if !strings.Contains(decision.Reason, "blocking result from objector") {
				t.Fatalf("decision reason = %q, want objector's blocking result", decision.Reason)
			}
			if len(gh.merges) != 0 {
				t.Fatalf("merge calls = %+v, want none", gh.merges)
			}
		})
	}
}

func TestPolicyMergeGateSameReviewerLaterVerdictSupersedesObjection(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	basePayload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
	}
	objection := basePayload
	objection.Result = &AgentResult{Decision: "changes_requested", Summary: "must fix"}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-z-objection", Agent: "reviewer", Type: "review"}, objection)
	setMergeGateJobTimestamps(t, store, "review-z-objection", "2026-07-31 12:00:00")

	approval := basePayload
	approval.Result = &AgentResult{Decision: "approved", Summary: "objection resolved"}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-a-approval", Agent: "reviewer", Type: "review"}, approval)
	setMergeGateJobTimestamps(t, store, "review-a-approval", "2026-07-31 12:01:00")

	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v, want later verdict from same reviewer to supersede objection", decision)
	}
}

func TestPolicyMergeGateDifferentReviewerCannotSupersedeObjection(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	basePayload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
	}
	objection := basePayload
	objection.Result = &AgentResult{Decision: "changes_requested", Summary: "must fix"}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-a-objection", Agent: "objector", Type: "review"}, objection)
	setMergeGateJobTimestamps(t, store, "review-a-objection", "2026-07-31 12:00:00")

	approval := basePayload
	approval.Result = &AgentResult{Decision: "approved", Summary: "ready"}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-z-approval", Agent: "approver", Type: "review"}, approval)
	setMergeGateJobTimestamps(t, store, "review-z-approval", "2026-07-31 12:01:00")

	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want different reviewer's objection to remain blocking", decision)
	}
	if !strings.Contains(decision.Reason, "blocking result from objector") {
		t.Fatalf("decision reason = %q, want objector's blocking result", decision.Reason)
	}
}

func TestPolicyMergeGateNonEvidenceVerdictDoesNotSupersedeObjection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision string
	}{
		{name: "skipped", decision: "skipped"},
		{name: "empty"},
		{name: "unknown future decision", decision: "future_verdict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			basePayload := JobPayload{
				Repo:        "gitmoot/gitmoot",
				PullRequest: 9,
				HeadSHA:     "head123",
				TaskID:      "task-9",
				ReviewRound: "review-1",
			}
			objection := basePayload
			objection.Result = &AgentResult{Decision: "changes_requested", Summary: "must fix"}
			insertIndependentMergeGateReview(t, store, db.Job{ID: "review-objector-objection", Agent: "objector", Type: "review"}, objection)
			setMergeGateJobTimestamps(t, store, "review-objector-objection", "2026-07-31 12:00:00")

			nonEvidence := basePayload
			nonEvidence.Result = &AgentResult{Decision: tc.decision, Summary: "no replacement evidence"}
			insertIndependentMergeGateReview(t, store, db.Job{ID: "review-objector-later", Agent: "objector", Type: "review"}, nonEvidence)
			setMergeGateJobTimestamps(t, store, "review-objector-later", "2026-07-31 12:01:00")

			approval := basePayload
			approval.Result = &AgentResult{Decision: "approved", Summary: "ready"}
			insertIndependentMergeGateReview(t, store, db.Job{ID: "review-approver", Agent: "approver", Type: "review"}, approval)
			setMergeGateJobTimestamps(t, store, "review-approver", "2026-07-31 12:02:00")

			mergeable := true
			gh := &fakeMergeGateGitHub{
				pr: github.PullRequest{
					Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
					HeadSHA: "head123", Mergeable: &mergeable,
				},
				status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
				checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
				mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
			}
			gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

			decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
				t.Fatalf("decision = %+v, want non-evidence decision %q to leave objection blocking", decision, tc.decision)
			}
			if !strings.Contains(decision.Reason, "blocking result from objector") {
				t.Fatalf("decision reason = %q, want objector's blocking result", decision.Reason)
			}
			if len(gh.merges) != 0 {
				t.Fatalf("merge calls = %+v, want none", gh.merges)
			}
		})
	}
}

func TestPolicyMergeGateWaitsForQueuedReviewAtEvaluatedHead(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-approved", agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-queued", agent: "reviewer-b", state: JobQueued,
		recorded: "2026-07-31 12:01:00",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Merged || decision.EscalateMergeGateMiss {
		t.Fatalf("decision = %+v, want queued review to wait without escalating", decision)
	}
	if !strings.Contains(decision.Reason, "waiting for reviewer reviewer-b") ||
		!strings.Contains(decision.Reason, "review-queued") {
		t.Fatalf("decision reason = %q, want queued reviewer and job", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateWaitsForRunningReviewAtEvaluatedHead(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-approved", agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-running", agent: "reviewer-b", state: JobRunning,
		recorded: "2026-07-31 12:01:00",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Merged || decision.EscalateMergeGateMiss {
		t.Fatalf("decision = %+v, want running review to wait without escalating", decision)
	}
	if !strings.Contains(decision.Reason, "waiting for reviewer reviewer-b") ||
		!strings.Contains(decision.Reason, "review-running") {
		t.Fatalf("decision reason = %q, want running reviewer and job", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateLatestQueuedReviewWaitsOverEarlierObjection(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-objected", agent: "reviewer-a", hasResult: true, decision: "changes_requested",
		recorded: "2026-07-31 12:00:00",
	})
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-requeued", agent: "reviewer-a", state: JobQueued,
		recorded: "2026-07-31 12:01:00",
	})
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-other-approved", agent: "reviewer-b", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:02:00",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Merged || decision.EscalateMergeGateMiss {
		t.Fatalf("decision = %+v, want latest queued review to wait", decision)
	}
	if !strings.Contains(decision.Reason, "waiting for reviewer reviewer-a") ||
		!strings.Contains(decision.Reason, "review-requeued") {
		t.Fatalf("decision reason = %q, want latest queued reviewer and job", decision.Reason)
	}
	if strings.Contains(decision.Reason, "blocking result") {
		t.Fatalf("decision reason = %q, stale objection must not override latest queued slot", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateParksFailedReviewerAtEvaluatedHead(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-approved", agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 18:01:24",
	})
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id:    "local-review-joltra-sol-review-18c723c7768aa61d",
		agent: "joltra-sol-review", state: JobFailed,
		recorded: "2026-07-31 18:11:56",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want failed reviewer parked", decision)
	}
	if !strings.Contains(decision.Reason, "crashed reviewer joltra-sol-review") ||
		!strings.Contains(decision.Reason, "local-review-joltra-sol-review-18c723c7768aa61d") {
		t.Fatalf("decision reason = %q, want crashed reviewer and job", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateParksCancelledReviewerAtEvaluatedHead(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-approved", agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-cancelled", agent: "reviewer-b", state: JobCancelled,
		recorded: "2026-07-31 12:01:00",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want cancelled reviewer parked", decision)
	}
	if !strings.Contains(decision.Reason, "crashed reviewer reviewer-b") ||
		!strings.Contains(decision.Reason, "review-cancelled") {
		t.Fatalf("decision reason = %q, want crashed reviewer and cancelled job", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateParksAbstainingReviewerAtEvaluatedHead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision string
	}{
		{name: "skipped", decision: "skipped"},
		{name: "unknown decision", decision: "future_unseen_verdict"},
		{name: "empty decision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: "review-approved", agent: "reviewer-a", hasResult: true, decision: "approved",
				recorded: "2026-07-31 12:00:00",
			})
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: "review-abstained", agent: "reviewer-b", hasResult: true, decision: tc.decision,
				recorded: "2026-07-31 12:01:00",
			})

			decision, err := gate.Evaluate(context.Background(), request)

			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
				t.Fatalf("decision = %+v, want abstaining reviewer parked", decision)
			}
			if !strings.Contains(decision.Reason, "abstaining reviewer reviewer-b") ||
				!strings.Contains(decision.Reason, "review-abstained") {
				t.Fatalf("decision reason = %q, want abstaining reviewer and job", decision.Reason)
			}
			if strings.Contains(decision.Reason, "crashed reviewer") {
				t.Fatalf("decision reason = %q, abstention must not use crash label", decision.Reason)
			}
			if len(gh.merges) != 0 {
				t.Fatalf("merge calls = %+v, want none", gh.merges)
			}
		})
	}
}

func TestPolicyMergeGateUsesLatestReviewJobPerReviewer(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "workflow-1hulo51pzm01f", agent: "joltra-sol-review",
		hasResult: true, decision: "approved", recorded: "2026-07-31 18:01:24",
	})
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id:    "local-review-joltra-sol-review-18c723c7768aa61d",
		agent: "joltra-sol-review", state: JobFailed, recorded: "2026-07-31 18:11:56",
	})
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id:    "local-review-joltra-sol-review-18c7245ac25c6980",
		agent: "joltra-sol-review", hasResult: true, decision: "changes_requested",
		recorded: "2026-07-31 18:22:29",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want latest changes_requested verdict to block", decision)
	}
	if !strings.Contains(decision.Reason, "blocking result from joltra-sol-review") {
		t.Fatalf("decision reason = %q, want latest reviewer's blocking result", decision.Reason)
	}
	if strings.Contains(decision.Reason, "crashed reviewer") {
		t.Fatalf("decision reason = %q, stale failed job must not park latest slot", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateLatestCrashOverridesStaleApproval(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-z-stale-approved", agent: "reviewer-a",
		hasResult: true, decision: "approved", recorded: "2026-07-31 12:00:00",
	})
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-a-latest-failed", agent: "reviewer-a",
		state: JobFailed, recorded: "2026-07-31 12:01:00",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want latest crashed reviewer parked", decision)
	}
	if !strings.Contains(decision.Reason, "crashed reviewer reviewer-a") ||
		!strings.Contains(decision.Reason, "review-a-latest-failed") {
		t.Fatalf("decision reason = %q, want latest crashed reviewer and job", decision.Reason)
	}
	if strings.Contains(decision.Reason, "review-z-stale-approved") {
		t.Fatalf("decision reason = %q, stale approval must not govern reviewer slot", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateRequeueClearsCrashedReviewerPark(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-other-approved", agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-crashed", agent: "reviewer-b", state: JobFailed,
		recorded: "2026-07-31 12:01:00",
	})

	parked, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("first Evaluate returned error: %v", err)
	}
	if !parked.LeaveOpen || !parked.EscalateMergeGateMiss || parked.Merged {
		t.Fatalf("first decision = %+v, want crashed reviewer parked", parked)
	}
	if !strings.Contains(parked.Reason, "crashed reviewer reviewer-b") ||
		!strings.Contains(parked.Reason, "review-crashed") {
		t.Fatalf("first decision reason = %q, want crashed reviewer and job", parked.Reason)
	}

	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-requeued-approved", agent: "reviewer-b", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:02:00",
	})
	merged, err := gate.Evaluate(context.Background(), request)
	if err != nil {
		t.Fatalf("second Evaluate returned error: %v", err)
	}
	if !merged.Merged {
		t.Fatalf("second decision = %+v, want later approval to clear reviewer slot", merged)
	}
	if len(gh.merges) != 1 {
		t.Fatalf("merge calls = %+v, want one after requeue approval", gh.merges)
	}
}

func TestPolicyMergeGateIgnoresReviewJobsAtStaleHeadForQuorum(t *testing.T) {
	store, _, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-current-approved", agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-stale-failed", agent: "reviewer-b", state: JobFailed, headSHA: "stale123",
		recorded: "2026-07-31 12:01:00",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v, want stale-head failed review excluded from quorum", decision)
	}
}

func TestPolicyMergeGateParksParentApprovalWhenDelegationChildrenFailed(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	const parentID = "workflow-cdz4fabzacb4"
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: parentID, agent: "g6-review-sol", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})
	for _, delegationID := range []string{
		"lens-anti-drift-correctness",
		"lens-compile-exhaustiveness",
		"lens-tests-regressions",
	} {
		insertMergeGateDelegationChild(t, store, parentID, delegationID, JobFailed, nil)
	}

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want failed delegated review evidence parked", decision)
	}
	if !strings.Contains(decision.Reason, "delegated review parent "+parentID) {
		t.Fatalf("decision reason = %q, want parent job id", decision.Reason)
	}
	for _, delegationID := range []string{
		"lens-anti-drift-correctness",
		"lens-compile-exhaustiveness",
		"lens-tests-regressions",
	} {
		childID := parentID + "/delegation/" + delegationID
		if !strings.Contains(decision.Reason, childID) {
			t.Fatalf("decision reason = %q, want failed child %s", decision.Reason, childID)
		}
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateAcceptsHeadlessIntegrationParentWhenDelegationChildrenSucceeded(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	const parentID = "review-parent-healthy"
	insertCompletedJob(t, store, db.Job{
		ID:           parentID,
		Agent:        "reviewer-a",
		Type:         "review",
		DelegationID: "verify-parent",
	}, JobPayload{
		Repo:         "gitmoot/gitmoot",
		PullRequest:  9,
		TaskID:       "task-9",
		ReviewRound:  "review-1",
		DelegationID: "verify-parent",
		WorktreePath: "/tmp/gitmoot/integration-verify-parent",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "integration review synthesized surviving evidence",
		},
	})
	for _, delegationID := range []string{"correctness", "compile", "tests"} {
		insertMergeGateDelegationChild(t, store, parentID, delegationID, JobSucceeded, &AgentResult{
			Decision: "approved",
			Summary:  "delegated evidence survived",
		})
	}

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v, want healthy delegated review to merge", decision)
	}
	if len(gh.merges) != 1 {
		t.Fatalf("merge calls = %+v, want one", gh.merges)
	}
}

func TestPolicyMergeGateFallbackParksHeadlessIntegrationParentWhenDelegationChildrenFailed(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	const parentID = "review-parent-fallback-failed"
	insertMergeGateHeadlessIntegrationParent(t, store, parentID)
	for _, delegationID := range []string{"correctness", "compile", "tests"} {
		insertMergeGateDelegationChild(t, store, parentID, delegationID, JobFailed, nil)
	}

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want fallback approval with failed children parked", decision)
	}
	if !strings.Contains(decision.Reason, "delegated review parent "+parentID) {
		t.Fatalf("decision reason = %q, want fallback parent job id", decision.Reason)
	}
	for _, delegationID := range []string{"correctness", "compile", "tests"} {
		childID := parentID + "/delegation/" + delegationID
		if !strings.Contains(decision.Reason, childID) {
			t.Fatalf("decision reason = %q, want failed child %s", decision.Reason, childID)
		}
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateParksHeadlessIntegrationParentWhenChildrenSkipped(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	const parentID = "review-parent-skipped"
	insertMergeGateHeadlessIntegrationParent(t, store, parentID)
	for _, delegationID := range []string{"correctness", "compile", "tests"} {
		insertMergeGateDelegationChild(t, store, parentID, delegationID, JobSucceeded, &AgentResult{
			Decision: "skipped",
			Summary:  "abstained",
		})
	}

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want all-skipped delegation evidence parked", decision)
	}
	if !strings.Contains(decision.Reason, "delegated review parent "+parentID) ||
		!strings.Contains(decision.Reason, "(skipped)") {
		t.Fatalf("decision reason = %q, want skipped children identified as no evidence", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateRejectsUnrecognizedDelegationEvidence(t *testing.T) {
	tests := []struct {
		name     string
		state    JobState
		decision string
	}{
		{name: "state", state: JobState("paused"), decision: "approved"},
		{name: "decision", state: JobSucceeded, decision: "future-review-outcome"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			parentID := "review-parent-unrecognized-" + tc.name
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: parentID, agent: "reviewer-a", hasResult: true, decision: "approved",
				recorded: "2026-07-31 12:00:00",
			})
			insertMergeGateDelegationChild(t, store, parentID, "unrecognized", tc.state, &AgentResult{
				Decision: tc.decision,
				Summary:  "synthetic unrecognized evidence",
			})

			decision, err := gate.Evaluate(context.Background(), request)

			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
				t.Fatalf("decision = %+v, want unrecognized delegation evidence refused", decision)
			}
			childID := parentID + "/delegation/unrecognized"
			if !strings.Contains(decision.Reason, "unrecognized delegation evidence") ||
				!strings.Contains(decision.Reason, childID) {
				t.Fatalf("decision reason = %q, want unrecognized child %s", decision.Reason, childID)
			}
			if len(gh.merges) != 0 {
				t.Fatalf("merge calls = %+v, want none", gh.merges)
			}
		})
	}
}

func TestPolicyMergeGateRejectsUnrecognizedDelegationEvidenceAlongsideHealthyChild(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	const parentID = "review-parent-mixed-unrecognized"
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: parentID, agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})
	insertMergeGateDelegationChild(t, store, parentID, "healthy", JobSucceeded, &AgentResult{
		Decision: "approved",
		Summary:  "surviving delegated evidence",
	})
	insertMergeGateDelegationChild(t, store, parentID, "unrecognized", JobState("paused"), &AgentResult{
		Decision: "approved",
		Summary:  "unrecognized child state",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want mixed unrecognized delegation evidence refused", decision)
	}
	unrecognizedChildID := parentID + "/delegation/unrecognized"
	if !strings.Contains(decision.Reason, "unrecognized delegation evidence") ||
		!strings.Contains(decision.Reason, unrecognizedChildID) {
		t.Fatalf("decision reason = %q, want unrecognized child %s", decision.Reason, unrecognizedChildID)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateDoesNotCountNonDelegationChildAsEvidence(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	const parentID = "review-parent-nondelegation-sibling"
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: parentID, agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})
	insertMergeGateDelegationChild(t, store, parentID, "skipped-delegation", JobSucceeded, &AgentResult{
		Decision: "skipped",
		Summary:  "delegated child abstained",
	})
	insertMergeGateDelegationChild(t, store, parentID, "", JobSucceeded, &AgentResult{
		Decision: "approved",
		Summary:  "ordinary child is not delegation evidence",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want non-delegation child excluded from evidence", decision)
	}
	skippedChildID := parentID + "/delegation/skipped-delegation"
	if !strings.Contains(decision.Reason, "no surviving delegation evidence") ||
		!strings.Contains(decision.Reason, skippedChildID) {
		t.Fatalf("decision reason = %q, want skipped delegated child %s", decision.Reason, skippedChildID)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateRejectsMalformedDelegationEvidence(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	const (
		parentID = "review-parent-malformed-child"
		childID  = parentID + "/delegation/malformed"
	)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: parentID, agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID:           childID,
		Agent:        "malformed",
		Type:         "ask",
		State:        string(JobSucceeded),
		Payload:      "{not-json",
		ParentJobID:  parentID,
		DelegationID: "malformed",
	}, db.JobEvent{Kind: string(JobSucceeded), Message: "malformed delegation fixture"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want malformed delegation evidence refused", decision)
	}
	if !strings.Contains(decision.Reason, "unrecognized delegation evidence") ||
		!strings.Contains(decision.Reason, childID+" (malformed result)") {
		t.Fatalf("decision reason = %q, want malformed child %s", decision.Reason, childID)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateWaitsForActiveDelegationChild(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state JobState
	}{
		{name: "queued", state: JobQueued},
		{name: "running", state: JobRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			parentID := "review-parent-active-" + tc.name
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: parentID, agent: "reviewer-a", hasResult: true, decision: "approved",
				recorded: "2026-07-31 12:00:00",
			})
			insertMergeGateDelegationChild(t, store, parentID, tc.name, tc.state, nil)

			decision, err := gate.Evaluate(context.Background(), request)

			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if decision.Merged || decision.LeaveOpen || decision.EscalateMergeGateMiss {
				t.Fatalf("decision = %+v, want active delegation child to wait without escalating", decision)
			}
			childID := parentID + "/delegation/" + tc.name
			if !strings.Contains(decision.Reason, "waiting for delegated review parent "+parentID) ||
				!strings.Contains(decision.Reason, childID+" ("+string(tc.state)+")") {
				t.Fatalf("decision reason = %q, want waiting reason with active child %s", decision.Reason, childID)
			}
			if len(gh.merges) != 0 {
				t.Fatalf("merge calls = %+v, want none", gh.merges)
			}
		})
	}
}

func TestPolicyMergeGateBlocksParentApprovalWhenDelegationChildRequestsChanges(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	const (
		parentID = "review-parent-blocking-child"
		childID  = parentID + "/delegation/objector"
	)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: parentID, agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})
	insertMergeGateDelegationChild(t, store, parentID, "objector", JobSucceeded, &AgentResult{
		Decision: "changes_requested",
		Summary:  "delegated review found a blocker",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want blocking delegation verdict to stop merge", decision)
	}
	if !strings.Contains(decision.Reason, "blocking delegation evidence") ||
		!strings.Contains(decision.Reason, childID+" (changes_requested)") {
		t.Fatalf("decision reason = %q, want blocking child %s", decision.Reason, childID)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateParksParentApprovalWhenDelegationChildImplemented(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	const (
		parentID = "review-parent-implemented-child"
		childID  = parentID + "/delegation/implementer"
	)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: parentID, agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})
	insertMergeGateDelegationChild(t, store, parentID, "implementer", JobSucceeded, &AgentResult{
		Decision: "implemented",
		Summary:  "implementation is not review evidence",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want implemented delegation result parked as abstention", decision)
	}
	if !strings.Contains(decision.Reason, "abstaining delegation children") ||
		!strings.Contains(decision.Reason, childID+" (implemented)") {
		t.Fatalf("decision reason = %q, want abstaining child %s", decision.Reason, childID)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateAcceptedDelegationChildDoesNotEraseAdverseSibling(t *testing.T) {
	tests := []struct {
		name            string
		adverseState    JobState
		adverseResult   *AgentResult
		wantReasonClass string
		wantDetail      string
	}{
		{
			name:            "blocking",
			adverseState:    JobSucceeded,
			adverseResult:   &AgentResult{Decision: "changes_requested", Summary: "blocker"},
			wantReasonClass: "blocking delegation evidence",
			wantDetail:      "(changes_requested)",
		},
		{
			name:            "crashed",
			adverseState:    JobFailed,
			wantReasonClass: "crashed delegation children",
			wantDetail:      "(failed)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			parentID := "review-parent-mixed-" + tc.name
			adverseID := parentID + "/delegation/adverse"
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: parentID, agent: "reviewer-a", hasResult: true, decision: "approved",
				recorded: "2026-07-31 12:00:00",
			})
			insertMergeGateDelegationChild(t, store, parentID, "approved", JobSucceeded, &AgentResult{
				Decision: "approved",
				Summary:  "one child approved",
			})
			insertMergeGateDelegationChild(t, store, parentID, "adverse", tc.adverseState, tc.adverseResult)

			decision, err := gate.Evaluate(context.Background(), request)

			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
				t.Fatalf("decision = %+v, want adverse sibling to outrank accepted child", decision)
			}
			if !strings.Contains(decision.Reason, tc.wantReasonClass) ||
				!strings.Contains(decision.Reason, adverseID+" "+tc.wantDetail) {
				t.Fatalf("decision reason = %q, want adverse child %s", decision.Reason, adverseID)
			}
			if len(gh.merges) != 0 {
				t.Fatalf("merge calls = %+v, want none", gh.merges)
			}
		})
	}
}

func TestPolicyMergeGateWinningDelegationOutcomeNamesSubordinateObligations(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	const (
		parentID       = "review-parent-mixed-naming"
		blockingID     = parentID + "/delegation/blocking"
		unrecognizedID = parentID + "/delegation/unrecognized"
	)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: parentID, agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})
	insertMergeGateDelegationChild(t, store, parentID, "blocking", JobSucceeded, &AgentResult{
		Decision: "blocked",
		Summary:  "delegated review blocked",
	})
	insertMergeGateDelegationChild(t, store, parentID, "unrecognized", JobSucceeded, &AgentResult{
		Decision: "future-review-outcome",
		Summary:  "unknown review outcome",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || decision.Merged {
		t.Fatalf("decision = %+v, want blocking outcome to win mixed delegation set", decision)
	}
	for _, want := range []string{
		"blocking delegation evidence",
		blockingID + " (blocked)",
		"unrecognized children:",
		unrecognizedID + ` (unrecognized decision "future-review-outcome")`,
	} {
		if !strings.Contains(decision.Reason, want) {
			t.Fatalf("decision reason = %q, want %q", decision.Reason, want)
		}
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

func TestPolicyMergeGateDelegatedReviewEvidenceEnumeration(t *testing.T) {
	type outcome string
	const (
		satisfies        outcome = "SAT"
		waits            outcome = "WAIT"
		blocks           outcome = "BLOCK"
		parksCrashed     outcome = "PARK_C"
		parksAbstaining  outcome = "PARK_A"
		parksUnknown     outcome = "PARK_U"
		parksCouldNotRun outcome = "PARK_STATE_BLOCKED_COULD_NOT_RUN"
	)
	type childFixture struct {
		label     string
		state     JobState
		decision  string
		nilResult bool
		malformed bool
	}
	type enumerationRow struct {
		id       string
		children []childFixture
		want     outcome
	}

	child := func(label string, state JobState, decision string) childFixture {
		return childFixture{label: label, state: state, decision: decision}
	}
	sat := func(label string) childFixture {
		return child(label, JobSucceeded, "approved")
	}
	wait := func(label string) childFixture {
		return child(label, JobRunning, "")
	}
	block := func(label string) childFixture {
		return child(label, JobSucceeded, "blocked")
	}
	unknown := func(label string) childFixture {
		return child(label, JobSucceeded, "future-review-outcome")
	}
	crashed := func(label string) childFixture {
		return child(label, JobFailed, "")
	}
	abstained := func(label string) childFixture {
		return child(label, JobSucceeded, "implemented")
	}
	couldNotRun := func(label string) childFixture {
		return child(label, JobBlocked, "")
	}

	rows := []enumerationRow{
		{id: "H01_QUEUED", children: []childFixture{child("queued", JobQueued, "")}, want: waits},
		{id: "H02_RUNNING", children: []childFixture{wait("running")}, want: waits},
		{id: "H03_APPROVED", children: []childFixture{sat("approved")}, want: satisfies},
		{id: "H04_CHANGES_REQUESTED", children: []childFixture{child("changes-requested", JobSucceeded, "changes_requested")}, want: blocks},
		{id: "H05_DECISION_BLOCKED_REFUSED", children: []childFixture{block("decision-blocked-refused")}, want: blocks},
		{id: "H06_DECISION_FAILED_REFUSED", children: []childFixture{child("decision-failed-refused", JobSucceeded, "failed")}, want: blocks},
		{id: "H07_SKIPPED", children: []childFixture{child("skipped", JobSucceeded, "skipped")}, want: parksAbstaining},
		{id: "H08_IMPLEMENTED", children: []childFixture{abstained("implemented")}, want: parksAbstaining},
		{id: "H09_EMPTY_DECISION", children: []childFixture{child("empty-decision", JobSucceeded, "")}, want: parksUnknown},
		{id: "H10_UNRECOGNIZED_DECISION", children: []childFixture{unknown("unrecognized-decision")}, want: parksUnknown},
		{id: "H11_NIL_RESULT", children: []childFixture{{label: "nil-result", state: JobSucceeded, nilResult: true}}, want: parksUnknown},
		{id: "H12_MALFORMED_PAYLOAD", children: []childFixture{{label: "malformed-payload", state: JobSucceeded, malformed: true}}, want: parksUnknown},
		{id: "H13_STATE_FAILED_CRASHED", children: []childFixture{crashed("state-failed-crashed")}, want: parksCrashed},
		{id: "H14_STATE_CANCELLED_CRASHED", children: []childFixture{child("state-cancelled-crashed", JobCancelled, "")}, want: parksCrashed},
		{id: "H15_STATE_BLOCKED_COULD_NOT_RUN", children: []childFixture{couldNotRun("state-blocked-could-not-run")}, want: parksCouldNotRun},
		{id: "H16_UNRECOGNIZED_STATE", children: []childFixture{child("unrecognized-state", JobState("paused"), "")}, want: parksUnknown},

		{id: "M01_SAT_PLUS_WAIT", children: []childFixture{sat("sat"), wait("wait")}, want: waits},
		{id: "M02_SAT_PLUS_BLOCK", children: []childFixture{sat("sat"), block("decision-blocked-refused")}, want: blocks},
		{id: "M03_SAT_PLUS_PARK_U", children: []childFixture{sat("sat"), unknown("park-u")}, want: parksUnknown},
		{id: "M04_SAT_PLUS_PARK_C", children: []childFixture{sat("sat"), crashed("park-c")}, want: parksCrashed},
		{id: "M05_SAT_PLUS_PARK_A", children: []childFixture{sat("sat"), abstained("park-a")}, want: parksAbstaining},
		{id: "M06_SAT_PLUS_STATE_BLOCKED_COULD_NOT_RUN", children: []childFixture{sat("sat"), couldNotRun("state-blocked-could-not-run")}, want: parksCouldNotRun},
		{id: "M07_WAIT_PLUS_BLOCK", children: []childFixture{wait("wait"), block("decision-blocked-refused")}, want: blocks},
		{id: "M08_WAIT_PLUS_PARK_U", children: []childFixture{wait("wait"), unknown("park-u")}, want: parksUnknown},
		{id: "M09_WAIT_PLUS_PARK_C", children: []childFixture{wait("wait"), crashed("park-c")}, want: waits},
		{id: "M10_WAIT_PLUS_PARK_A", children: []childFixture{wait("wait"), abstained("park-a")}, want: waits},
		{id: "M11_WAIT_PLUS_STATE_BLOCKED_COULD_NOT_RUN", children: []childFixture{wait("wait"), couldNotRun("state-blocked-could-not-run")}, want: waits},
		{id: "M12_BLOCK_PLUS_PARK_U", children: []childFixture{block("decision-blocked-refused"), unknown("park-u")}, want: blocks},
		{id: "M13_BLOCK_PLUS_PARK_C", children: []childFixture{block("decision-blocked-refused"), crashed("park-c")}, want: blocks},
		{id: "M14_BLOCK_PLUS_PARK_A", children: []childFixture{block("decision-blocked-refused"), abstained("park-a")}, want: blocks},
		{id: "M15_BLOCK_PLUS_STATE_BLOCKED_COULD_NOT_RUN", children: []childFixture{block("decision-blocked-refused"), couldNotRun("state-blocked-could-not-run")}, want: blocks},
		{id: "M16_PARK_U_PLUS_PARK_C", children: []childFixture{unknown("park-u"), crashed("park-c")}, want: parksUnknown},
		{id: "M17_PARK_U_PLUS_PARK_A", children: []childFixture{unknown("park-u"), abstained("park-a")}, want: parksUnknown},
		{id: "M18_PARK_U_PLUS_STATE_BLOCKED_COULD_NOT_RUN", children: []childFixture{unknown("park-u"), couldNotRun("state-blocked-could-not-run")}, want: parksUnknown},
		{id: "M19_PARK_C_PLUS_PARK_A", children: []childFixture{crashed("park-c"), abstained("park-a")}, want: parksCrashed},
		{id: "M20_PARK_C_PLUS_STATE_BLOCKED_COULD_NOT_RUN", children: []childFixture{crashed("park-c"), couldNotRun("state-blocked-could-not-run")}, want: parksCrashed},
		{id: "M21_PARK_A_PLUS_STATE_BLOCKED_COULD_NOT_RUN", children: []childFixture{abstained("park-a"), couldNotRun("state-blocked-could-not-run")}, want: parksAbstaining},
	}

	linkages := []struct {
		name         string
		parent       bool
		delegation   bool
		wantExcluded bool
	}{
		{name: "FULL_PARENT_AND_DELEGATION", parent: true, delegation: true},
		{name: "PARENT_ONLY_NO_DELEGATION", parent: true, wantExcluded: true},
		{name: "NEITHER_PARENT_NOR_DELEGATION", wantExcluded: true},
	}

	for rowIndex, row := range rows {
		row := row
		t.Run(row.id, func(t *testing.T) {
			for _, linkage := range linkages {
				linkage := linkage
				t.Run(linkage.name, func(t *testing.T) {
					store, gh, gate, request := newMergeGateQuorumScenario(t)
					parentID := fmt.Sprintf("review-parent-enumeration-%02d", rowIndex)
					insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
						id: parentID, agent: "reviewer-a", hasResult: true, decision: "approved",
						recorded: "2026-07-31 12:00:00",
					})

					var includedAdverseIDs []string
					for childIndex, fixture := range row.children {
						childID := fmt.Sprintf("%s/child/%02d-%s", parentID, childIndex, fixture.label)
						payload := "{not-json"
						jobType := "ask"
						if !fixture.malformed {
							var result *AgentResult
							if !fixture.nilResult {
								result = &AgentResult{Decision: fixture.decision, Summary: "enumerated delegation result"}
							}
							encoded, err := marshalPayload(JobPayload{
								Repo:        "gitmoot/gitmoot",
								PullRequest: 9,
								TaskID:      "task-9",
								Result:      result,
							})
							if err != nil {
								t.Fatalf("marshalPayload returned error: %v", err)
							}
							payload = encoded
							jobType = "review"
						}

						job := db.Job{
							ID:      childID,
							Agent:   fixture.label,
							Type:    jobType,
							State:   string(fixture.state),
							Payload: payload,
						}
						if linkage.parent {
							job.ParentJobID = parentID
						}
						if linkage.delegation {
							job.DelegationID = fixture.label
						}
						if err := store.CreateJobWithEvent(context.Background(), job, db.JobEvent{
							Kind: string(fixture.state), Message: "enumerated delegation fixture",
						}); err != nil {
							t.Fatalf("CreateJobWithEvent returned error: %v", err)
						}
						if !linkage.wantExcluded && !(fixture.state == JobSucceeded && fixture.decision == "approved") {
							includedAdverseIDs = append(includedAdverseIDs, childID)
						}
					}

					decision, err := gate.Evaluate(context.Background(), request)
					if err != nil {
						t.Fatalf("Evaluate returned error: %v", err)
					}

					want := row.want
					if linkage.wantExcluded {
						want = satisfies
					}
					switch want {
					case satisfies:
						if !decision.Merged || len(gh.merges) != 1 {
							t.Fatalf("decision = %+v merge calls = %+v, want SAT", decision, gh.merges)
						}
					case waits:
						if decision.Merged || decision.LeaveOpen || decision.EscalateMergeGateMiss ||
							!strings.Contains(decision.Reason, "waiting for delegated review parent") {
							t.Fatalf("decision = %+v, want WAIT", decision)
						}
					case blocks:
						if decision.Merged || !decision.LeaveOpen || !decision.EscalateMergeGateMiss ||
							!strings.Contains(decision.Reason, "blocking delegation evidence") {
							t.Fatalf("decision = %+v, want BLOCK", decision)
						}
					case parksUnknown:
						if decision.Merged || !decision.LeaveOpen || !decision.EscalateMergeGateMiss ||
							!strings.Contains(decision.Reason, "unrecognized delegation evidence") {
							t.Fatalf("decision = %+v, want PARK-U", decision)
						}
					case parksCrashed:
						if decision.Merged || !decision.LeaveOpen || !decision.EscalateMergeGateMiss ||
							!strings.Contains(decision.Reason, "crashed delegation children") {
							t.Fatalf("decision = %+v, want PARK-C", decision)
						}
					case parksAbstaining:
						if decision.Merged || !decision.LeaveOpen || !decision.EscalateMergeGateMiss ||
							!strings.Contains(decision.Reason, "abstaining delegation children") {
							t.Fatalf("decision = %+v, want PARK-A", decision)
						}
					case parksCouldNotRun:
						if decision.Merged || !decision.LeaveOpen || !decision.EscalateMergeGateMiss ||
							!strings.Contains(decision.Reason, "parked children:") {
							t.Fatalf("decision = %+v, want PARK STATE_BLOCKED_COULD_NOT_RUN", decision)
						}
					default:
						t.Fatalf("unknown expected outcome %q", want)
					}
					for _, childID := range includedAdverseIDs {
						if !strings.Contains(decision.Reason, childID) {
							t.Fatalf("decision reason = %q, want subordinate child %s", decision.Reason, childID)
						}
					}
				})
			}
		})
	}
}

func TestPolicyMergeGateLeavesOrdinaryReviewWithoutDelegationsUnaffected(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-ordinary", agent: "reviewer-a", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:00:00",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v, want ordinary review to remain merge-admissible", decision)
	}
	if len(gh.merges) != 1 {
		t.Fatalf("merge calls = %+v, want one", gh.merges)
	}
}

func TestPolicyMergeGateHeadlessIntegrationObjectionDoesNotMatchEveryHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	headlessObjection := JobPayload{
		Repo:         "gitmoot/gitmoot",
		PullRequest:  9,
		TaskID:       "task-9",
		ReviewRound:  "review-1",
		DelegationID: "verify-old",
		WorktreePath: "/tmp/gitmoot/integration-verify-old",
		Result:       &AgentResult{Decision: "changes_requested", Summary: "integration objection"},
	}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-headless-objection", Agent: "objector", Type: "review"}, headlessObjection)

	currentApproval := JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-2",
		Result:      &AgentResult{Decision: "approved", Summary: "current head approved"},
	}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-current-approval", Agent: "approver", Type: "review"}, currentApproval)

	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "head123", Mergeable: &mergeable,
		},
		status:      github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks:      []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
		mergeResult: github.MergeResult{Merged: true, SHA: "merge123"},
	}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged {
		t.Fatalf("decision = %+v, want headless integration objection excluded from strict-head pre-pass", decision)
	}
}

func TestPolicyMergeGateBlocksReviewForStaleHead(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || !strings.Contains(decision.Reason, "different head SHA") {
		t.Fatalf("decision = %+v", decision)
	}
	if gh.prCheckCalls != 0 || len(gh.checkRefs) != 1 || gh.checkRefs[0] != "head123" {
		t.Fatalf("check calls = pr:%d refs:%v; want exact current head", gh.prCheckCalls, gh.checkRefs)
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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.EscalateMergeGateMiss || !strings.Contains(decision.Reason, "does not record a head SHA") {
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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review", DelegationID: "verify-gate"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

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
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review", DelegationID: "verify-gate"}, JobPayload{
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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

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
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Ready || !strings.Contains(decision.Reason, "review") {
		t.Fatalf("decision = %+v", decision)
	}
}

type fakeMergeGateGitHub struct {
	pr           github.PullRequest
	status       github.CombinedStatus
	compare      github.CompareResult
	checks       []github.PullRequestCheck
	mergeResult  github.MergeResult
	mergeErr     error
	statusErr    error
	updateErr    error
	statuses     []github.CommitStatusInput
	merges       []github.MergePullRequestInput
	updates      []github.UpdatePullRequestBranchInput
	comments     []string
	operations   []string
	getCalls     int
	statusCalls  int
	compareCalls int
	checkCalls   int
	prCheckCalls int
	checkRefs    []string
	noChecks     bool
}

func (f *fakeMergeGateGitHub) GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error) {
	f.getCalls++
	return f.pr, nil
}

func (f *fakeMergeGateGitHub) GetCombinedStatus(context.Context, github.Repository, string) (github.CombinedStatus, error) {
	f.statusCalls++
	return f.status, nil
}

func (f *fakeMergeGateGitHub) CompareCommits(context.Context, github.Repository, string, string) (github.CompareResult, error) {
	f.compareCalls++
	if f.compare.Status == "" && f.compare.AheadBy == 0 && f.compare.BehindBy == 0 {
		return github.CompareResult{Status: "ahead", AheadBy: 1}, nil
	}
	return f.compare, nil
}

func (f *fakeMergeGateGitHub) ListPullRequestChecks(context.Context, github.Repository, int64) ([]github.PullRequestCheck, error) {
	f.prCheckCalls++
	return f.checks, nil
}

func (f *fakeMergeGateGitHub) ListCheckRunsForRef(_ context.Context, _ github.Repository, ref string) ([]github.PullRequestCheck, error) {
	f.checkCalls++
	f.checkRefs = append(f.checkRefs, ref)
	if !f.noChecks && f.checks == nil {
		return []github.PullRequestCheck{{Name: "ci", State: "SUCCESS", Bucket: "pass"}}, nil
	}
	return f.checks, nil
}

func (f *fakeMergeGateGitHub) CreateCommitStatus(_ context.Context, input github.CommitStatusInput) (github.CommitStatus, error) {
	f.statuses = append(f.statuses, input)
	f.operations = append(f.operations, "status:"+input.Context+":"+input.State)
	return github.CommitStatus{State: input.State, Context: input.Context}, f.statusErr
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
	f.operations = append(f.operations, "merge")
	return f.mergeResult, f.mergeErr
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

func setMergeGateJobTimestamps(t *testing.T, store *db.Store, jobID string, timestamp string) {
	t.Helper()
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("open store for timestamp update: %v", err)
	}
	defer raw.Close()
	result, err := raw.ExecContext(context.Background(),
		`UPDATE jobs SET created_at = ?, updated_at = ? WHERE id = ?`,
		timestamp, timestamp, jobID)
	if err != nil {
		t.Fatalf("set job %s timestamps: %v", jobID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		t.Fatalf("set job %s timestamps affected=%d, err=%v; want 1", jobID, affected, err)
	}
}
