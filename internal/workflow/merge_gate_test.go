package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/db/dbtest"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/reviewseverity"
	"github.com/gitmoot/gitmoot/internal/subprocess"
)

func insertIndependentMergeGateReview(t *testing.T, store *db.Store, reviewJob db.Job, reviewPayload JobPayload) {
	t.Helper()
	seedMergeGateFixtureAgent(t, store, reviewJob.Agent)
	implementingAgent := "implementer"
	if strings.TrimSpace(reviewJob.Agent) == implementingAgent {
		implementingAgent = "different-implementer"
	}
	implementPayload := reviewPayload
	implementPayload.ReviewRound = ""
	implementPayload.Result = &AgentResult{Decision: "implemented", Summary: "implemented"}
	seedMergeGateFixtureAgent(t, store, implementingAgent)
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
	// recorded sets created_at AND updated_at to one value. createdAt/updatedAt
	// set them INDEPENDENTLY, which is the only way to express a row retried in
	// place: `gitmoot job retry` keeps created_at and bumps updated_at, so the
	// earliest-created row can hold the newest verdict.
	recorded  string
	createdAt string
	updatedAt string
	// emptyRound reproduces a CLI-dispatched review: `gitmoot agent review` sets
	// HeadSHA and never sets ReviewRound.
	emptyRound bool
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

// seedMergeGateFixtureAgent delegates to the SHARED helper (#2004). It stays as
// a one-line wrapper only because this package's fixtures call it in a dozen
// places; the rule itself - one family per name, and never register a synthetic
// agent - lives in dbtest, so internal/cli and internal/daemon get the same one
// instead of a third copy that drifts.
func seedMergeGateFixtureAgent(t *testing.T, store *db.Store, name string) {
	t.Helper()
	dbtest.SeedGateFixtureAgent(t, store, name)
}

func insertMergeGateReviewFixture(t *testing.T, store *db.Store, fixture mergeGateReviewFixture) {
	t.Helper()
	seedMergeGateFixtureAgent(t, store, fixture.agent)
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
	if fixture.emptyRound {
		payload.ReviewRound = ""
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
	switch {
	case fixture.createdAt != "" || fixture.updatedAt != "":
		if fixture.recorded != "" {
			t.Fatalf("fixture %s sets recorded together with createdAt/updatedAt", fixture.id)
		}
		setMergeGateJobRecordedTimes(t, store, fixture.id, fixture.createdAt, fixture.updatedAt)
	case fixture.recorded != "":
		setMergeGateJobTimestamps(t, store, fixture.id, fixture.recorded)
	}
}

// insertMergeGateDelegationChild creates a delegation child AND records that
// delegation on the parent's stored result. Production always writes both: the
// engine dispatches children FROM the parent result's delegations[], so a parent
// with children and an empty delegations[] is a shape the product never
// produces. Fixtures that built it hid #1685 — the fan-out row is exactly the
// row whose delegations[] is populated.
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
	if strings.TrimSpace(delegationID) == "" {
		// An ordinary child with no delegation id was never declared by the parent.
		return
	}
	declareMergeGateParentDelegation(t, store, parentID, delegationID)
}

// declareMergeGateParentDelegation appends one delegation to the parent review's
// stored result, mirroring what the agent wrote before the engine fanned out.
//
// It RESTORES the parent's recorded timestamps afterwards. UpdateJobPayload bumps
// updated_at, and roundless review rows are ranked by exactly that column
// (reviewRoundKeyForJob), so a fixture that declares a delegation would otherwise
// silently make its parent the newest round and break tests about recency that
// have nothing to do with delegation. Adding evidence to a fixture must not move
// that fixture in time.
func declareMergeGateParentDelegation(t *testing.T, store *db.Store, parentID, delegationID string) {
	t.Helper()
	ctx := context.Background()
	parent, err := store.GetJob(ctx, parentID)
	if err != nil {
		t.Fatalf("GetJob(%s) returned error: %v", parentID, err)
	}
	payload, err := ParseJobPayload(parent.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(%s) returned error: %v", parentID, err)
	}
	if payload.Result == nil {
		return
	}
	payload.Result.Delegations = append(payload.Result.Delegations, Delegation{
		ID: delegationID, Agent: delegationID, Action: "review",
	})
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, parentID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload(%s) returned error: %v", parentID, err)
	}
	setMergeGateJobRecordedTimes(t, store, parentID, parent.CreatedAt, parent.UpdatedAt)
}

func insertMergeGateHeadlessIntegrationParent(t *testing.T, store *db.Store, parentID string) {
	t.Helper()
	insertCompletedJob(t, store, db.Job{
		ID:    parentID,
		Agent: "reviewer-a",
		Type:  "review",
		// Production shape: mailbox.Enqueue writes ParentJobID AND DelegationID for
		// every delegated review, so isDelegationChild is true for this row. The
		// quorum scenario's implement job is the delegator.
		ParentJobID:  "implement-job",
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
				{Context: GitmootMergeGateContext, State: "failure"},
			},
		},
		checks: []github.PullRequestCheck{
			{Name: GitmootMergeGateContext, Bucket: "fail", State: "FAILURE"},
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
	if !hasStatus(gh.statuses, GitmootMergeGateContext, "success") || hasStatus(gh.statuses, gitmootNoCIContext, "success") {
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

func TestPolicyMergeGateFencesReadyStateThroughExternalMerge(t *testing.T) {
	ctx := context.Background()
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-job", agent: "audit", decision: "approved", hasResult: true,
	})
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	request.Reviewer = "audit"
	request.ExpectedTaskState = string(TaskReadyToMerge)
	writer, err := db.OpenAlreadyMigrated(store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var blocked bool
	var blockErr error
	gh.beforeMerge = func() {
		blocked, blockErr = writer.BlockTaskWithEvent(ctx, db.Task{
			ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
			State: string(TaskBlocked),
		}, db.TaskEvent{
			Kind: "workflow_blocked", FromState: string(TaskReadyToMerge),
			Reason: "concurrent production block",
		})
	}
	decision, err := gate.Evaluate(ctx, request)
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged || len(gh.merges) != 1 {
		t.Fatalf("decision=%+v merges=%d, want one completed external merge", decision, len(gh.merges))
	}
	if blocked || !errors.Is(blockErr, db.ErrTaskStateClaimed) {
		t.Fatalf("concurrent block = blocked %v err %v, want durable claim conflict", blocked, blockErr)
	}
	task, taskErr := store.GetTask(ctx, "task-9")
	events, eventsErr := store.ListTaskEvents(ctx, "task-9")
	if taskErr != nil || eventsErr != nil || task.State != string(TaskMerged) {
		t.Fatalf("task=%+v events=%+v taskErr=%v eventsErr=%v", task, events, taskErr, eventsErr)
	}
	for _, event := range events {
		if event.Kind == "workflow_blocked" {
			t.Fatalf("events=%+v, concurrent block must not commit after merge claim", events)
		}
	}
}

func TestPolicyMergeGateRetainsClaimAcrossAmbiguousPostMergeConfirmation(t *testing.T) {
	const helperPathEnv = "GITMOOT_AMBIGUOUS_MERGE_CLAIM_HELPER_PATH"
	ctx := context.Background()
	if path := os.Getenv(helperPathEnv); path != "" {
		writer, err := db.OpenAlreadyMigrated(path)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		changed, _, err := writer.DisposeTask(ctx, "task-9", []string{string(TaskReadyToMerge)},
			string(TaskDismissed), "stale", "cross-process ambiguous-outcome disposal", "", "task_disposed", time.Now())
		if err == nil || changed || !strings.Contains(err.Error(), "claimed for an external merge") {
			t.Fatalf("cross-process disposal = changed %v err %v, want retained-claim rejection", changed, err)
		}
		return
	}

	store, base, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-job", agent: "audit", decision: "approved", hasResult: true,
	})
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	request.Reviewer = "audit"
	request.ExpectedTaskState = string(TaskReadyToMerge)
	base.getPullRequest = func(call int) (github.PullRequest, error) {
		if call == 2 {
			return github.PullRequest{}, errors.New("secondary confirmation unavailable")
		}
		return base.pr, nil
	}
	runner := &mergeConfirmationFailureRunner{onMerge: func() {
		base.pr.State = "closed"
		base.pr.Merged = true
		base.pr.MergeSHA = "merge-ambiguous"
	}}
	composed := &productionMergeGateGitHub{
		fakeMergeGateGitHub: base,
		mergeClient:         github.GhClient{Runner: runner},
	}
	gate.GitHub = composed

	decision, err := gate.Evaluate(ctx, request)
	if err == nil || !strings.Contains(err.Error(), "durable task ownership retained") {
		t.Fatalf("first Evaluate decision=%+v err=%v, want retained ambiguous outcome", decision, err)
	}
	if runner.calls != 2 || len(composed.merges) != 1 {
		t.Fatalf("production adapter calls=%d merge attempts=%d, want one command plus failed confirmation", runner.calls, len(composed.merges))
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPolicyMergeGateRetainsClaimAcrossAmbiguousPostMergeConfirmation$")
	cmd.Env = append(os.Environ(), helperPathEnv+"="+store.DatabasePath())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-process ambiguous-outcome writer: %v\n%s", err, output)
	}

	decision, err = gate.Evaluate(ctx, request)
	if err != nil || !decision.Merged || decision.MergeCommitSHA != "merge-ambiguous" {
		t.Fatalf("reconciliation decision=%+v err=%v, want recovered remote merge", decision, err)
	}
	task, taskErr := store.GetTask(ctx, "task-9")
	if taskErr != nil || task.State != string(TaskMerged) {
		t.Fatalf("task=%+v err=%v, want merged after reconciliation", task, taskErr)
	}
	if len(composed.merges) != 1 {
		t.Fatalf("merge attempts=%d, reconciliation must not issue a second merge", len(composed.merges))
	}
}

func TestPolicyMergeGateRetainsClaimForAcceptedQueuedMerge(t *testing.T) {
	ctx := context.Background()
	store, base, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-job", agent: "audit", decision: "approved", hasResult: true,
	})
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	writer, err := db.OpenAlreadyMigrated(store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	request.Reviewer = "audit"
	request.ExpectedTaskState = string(TaskReadyToMerge)
	runner := &queuedMergeRunner{}
	composed := &productionMergeGateGitHub{
		fakeMergeGateGitHub: base,
		mergeClient:         github.GhClient{Runner: runner},
	}
	gate.GitHub = composed

	decision, err := gate.Evaluate(ctx, request)
	if err != nil || !decision.Ready || decision.Merged || !strings.Contains(decision.Reason.Render(), "pending") {
		t.Fatalf("queued Evaluate decision=%+v err=%v, want accepted pending merge", decision, err)
	}
	if runner.calls != 2 || len(composed.merges) != 1 {
		t.Fatalf("production adapter calls=%d merge attempts=%d, want one accepted command and open-state confirmation",
			runner.calls, len(composed.merges))
	}
	changed, _, writeErr := writer.DisposeTask(ctx, "task-9", []string{string(TaskReadyToMerge)},
		string(TaskDismissed), "stale", "queued merge disposal", "", "task_disposed", time.Now())
	if writeErr == nil || changed || !strings.Contains(writeErr.Error(), "claimed for an external merge") {
		t.Fatalf("conflicting queued-merge disposal = changed %v err %v, want durable claim rejection", changed, writeErr)
	}

	base.pr.State = "closed"
	base.pr.Merged = true
	base.pr.MergeSHA = "merge-completed-from-queue"
	decision, err = gate.Evaluate(ctx, request)
	if err != nil || !decision.Merged || decision.MergeCommitSHA != "merge-completed-from-queue" {
		t.Fatalf("queued reconciliation decision=%+v err=%v, want recovered merge", decision, err)
	}
	task, taskErr := store.GetTask(ctx, "task-9")
	if taskErr != nil || task.State != string(TaskMerged) {
		t.Fatalf("task=%+v err=%v, want merged after queued reconciliation", task, taskErr)
	}
	if len(composed.merges) != 1 {
		t.Fatalf("merge attempts=%d, reconciliation must not issue a second merge", len(composed.merges))
	}
}

func TestPolicyMergeGateRetainsFailedMergeWhileRemoteOpenUntilClosure(t *testing.T) {
	ctx := context.Background()
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-job", agent: "audit", decision: "approved", hasResult: true,
	})
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	writer, err := db.OpenAlreadyMigrated(store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	request.Reviewer = "audit"
	request.ExpectedTaskState = string(TaskReadyToMerge)
	gh.mergeErr = errors.New("merge command failed after submission")

	decision, err := gate.Evaluate(ctx, request)
	if err == nil || !strings.Contains(err.Error(), "pull request remains open") ||
		!strings.Contains(err.Error(), "durable task ownership retained") {
		t.Fatalf("open confirmation decision=%+v err=%v, want retained unresolved merge", decision, err)
	}
	changed, _, writeErr := writer.DisposeTask(ctx, "task-9", []string{string(TaskReadyToMerge)},
		string(TaskDismissed), "stale", "open merge disposal", "", "task_disposed", time.Now())
	if writeErr == nil || changed || !strings.Contains(writeErr.Error(), "claimed for an external merge") {
		t.Fatalf("conflicting open-merge disposal = changed %v err %v, want durable claim rejection", changed, writeErr)
	}

	gh.pr.State = "closed"
	decision, err = gate.Evaluate(ctx, request)
	if err != nil || !strings.Contains(decision.Reason.Render(), "closed without being merged") {
		t.Fatalf("closed reconciliation decision=%+v err=%v, want terminal non-merge block", decision, err)
	}
	changed, current, writeErr := writer.DisposeTask(ctx, "task-9", []string{string(TaskReadyToMerge)},
		string(TaskDismissed), "stale", "terminal non-merge disposal", "", "task_disposed", time.Now())
	if writeErr != nil || !changed || current != string(TaskDismissed) {
		t.Fatalf("terminal non-merge disposal = changed %v current %q err %v, want released claim", changed, current, writeErr)
	}
}

func TestPolicyMergeGateCompletesClaimWhenPostErrorConfirmationShowsMerged(t *testing.T) {
	ctx := context.Background()
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-job", agent: "audit", decision: "approved", hasResult: true,
	})
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	request.Reviewer = "audit"
	request.ExpectedTaskState = string(TaskReadyToMerge)
	gh.mergeErr = errors.New("fetch merged pull request: HTTP 502")
	gh.beforeMerge = func() {
		gh.pr.State = "closed"
		gh.pr.Merged = true
		gh.pr.MergeSHA = "merge-confirmed-after-error"
	}

	decision, err := gate.Evaluate(ctx, request)
	if err != nil || !decision.Merged || decision.MergeCommitSHA != "merge-confirmed-after-error" {
		t.Fatalf("Evaluate decision=%+v err=%v, want authoritative post-error merge completion", decision, err)
	}
	task, taskErr := store.GetTask(ctx, "task-9")
	if taskErr != nil || task.State != string(TaskMerged) {
		t.Fatalf("task=%+v err=%v, want merged claim completion", task, taskErr)
	}
}

func TestPolicyMergeGateRenewsClaimBeyondLeaseAcrossProcess(t *testing.T) {
	const helperPathEnv = "GITMOOT_RENEWED_MERGE_CLAIM_HELPER_PATH"
	ctx := context.Background()
	if path := os.Getenv(helperPathEnv); path != "" {
		writer, err := db.OpenAlreadyMigrated(path)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		changed, _, err := writer.DisposeTask(ctx, "task-9", []string{string(TaskReadyToMerge)},
			string(TaskDismissed), "stale", "cross-process lease-expiry disposal", "", "task_disposed", time.Now())
		if err == nil || changed || !strings.Contains(err.Error(), "claimed for an external merge") {
			t.Fatalf("cross-process disposal = changed %v err %v, want renewed-claim rejection", changed, err)
		}
		return
	}

	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-job", agent: "audit", decision: "approved", hasResult: true,
	})
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	request.Reviewer = "audit"
	request.ExpectedTaskState = string(TaskReadyToMerge)
	mergeStarted := make(chan struct{})
	allowMerge := make(chan struct{})
	gh.beforeMerge = func() {
		close(mergeStarted)
		<-allowMerge
	}
	ticks := make(chan time.Time)
	renewed := make(chan struct{}, 1)
	gate.taskClaimTTL = 4 * time.Second
	gate.taskClaimRenewalInterval = 2 * time.Second
	gate.taskClaimRenewalTicks = func(context.Context, time.Duration) <-chan time.Time {
		return ticks
	}
	gate.afterTaskClaimRenewal = func() {
		renewed <- struct{}{}
	}
	type evaluateResult struct {
		decision MergeDecision
		err      error
	}
	evaluated := make(chan evaluateResult, 1)
	go func() {
		decision, err := gate.Evaluate(ctx, request)
		evaluated <- evaluateResult{decision: decision, err: err}
	}()
	select {
	case <-mergeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("merge did not reach the production external-call boundary")
	}
	time.Sleep(2100 * time.Millisecond)
	ticks <- time.Now()
	select {
	case <-renewed:
	case <-time.After(5 * time.Second):
		t.Fatal("durable claim renewal did not complete")
	}
	time.Sleep(2100 * time.Millisecond)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPolicyMergeGateRenewsClaimBeyondLeaseAcrossProcess$")
	cmd.Env = append(os.Environ(), helperPathEnv+"="+store.DatabasePath())
	if output, err := cmd.CombinedOutput(); err != nil {
		close(allowMerge)
		t.Fatalf("cross-process lease-expiry writer: %v\n%s", err, output)
	}
	close(allowMerge)
	result := <-evaluated
	if result.err != nil || !result.decision.Merged {
		t.Fatalf("Evaluate decision=%+v err=%v, want successful merge after lease renewal", result.decision, result.err)
	}
	task, taskErr := store.GetTask(ctx, "task-9")
	if taskErr != nil || task.State != string(TaskMerged) {
		t.Fatalf("task=%+v err=%v, want merged after renewed in-flight claim", task, taskErr)
	}
}

func TestHandlePullRequestOpenedFencesNoReviewerMergeFromPROpenState(t *testing.T) {
	ctx := context.Background()
	store, gh, gate, _ := newMergeGateQuorumScenario(t)
	seedAgent(t, store, "implementer", []string{"implement"}, "gitmoot/gitmoot")
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-job", agent: "audit", decision: "approved", hasResult: true,
	})
	writer, err := db.OpenAlreadyMigrated(store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var blocked bool
	var blockErr error
	gate.Git.(*fakeMergeGateGit).onClean = func() {
		blocked, blockErr = writer.BlockTaskWithEvent(ctx, db.Task{
			ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
			State: string(TaskBlocked),
		}, db.TaskEvent{
			Kind: "workflow_blocked", FromState: string(TaskPullRequestOpen),
			Reason: "concurrent PR-open block",
		})
	}
	engine := testEngine(store)
	engine.MergeGate = gate

	err = engine.HandlePullRequestOpened(ctx, PullRequestEvent{
		Repo: "gitmoot/gitmoot", Branch: "task-9", PullRequest: 9,
		HeadSHA: "head123", TaskID: "task-9", LeadAgent: "implementer",
	})
	if !errors.Is(err, ErrMergeTaskStateChanged) {
		t.Fatalf("HandlePullRequestOpened error = %v, want PR-open state fence conflict", err)
	}
	if !blocked || blockErr != nil {
		t.Fatalf("concurrent block = blocked %v err %v, want committed PR-open block", blocked, blockErr)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("external merges = %d, want none after PR-open state changed", len(gh.merges))
	}
	task, taskErr := store.GetTask(ctx, "task-9")
	if taskErr != nil || task.State != string(TaskBlocked) {
		t.Fatalf("task=%+v err=%v, want blocked", task, taskErr)
	}
}

func TestAdvanceApprovedReviewFencesMergeFromReviewingState(t *testing.T) {
	ctx := context.Background()
	store, gh, gate, _ := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-job", agent: "audit", decision: "approved", hasResult: true,
	})
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReviewing),
	}); err != nil {
		t.Fatal(err)
	}
	writer, err := db.OpenAlreadyMigrated(store.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var blocked bool
	var blockErr error
	gate.Git.(*fakeMergeGateGit).onClean = func() {
		blocked, blockErr = writer.BlockTaskWithEvent(ctx, db.Task{
			ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
			State: string(TaskBlocked),
		}, db.TaskEvent{
			Kind: "workflow_blocked", FromState: string(TaskReviewing),
			Reason: "concurrent review completion block",
		})
	}
	engine := testEngine(store)
	engine.MergeGate = gate

	err = engine.AdvanceJob(ctx, "review-job")
	if !errors.Is(err, ErrMergeTaskStateChanged) {
		t.Fatalf("AdvanceJob error = %v, want reviewing-state fence conflict", err)
	}
	if !blocked || blockErr != nil {
		t.Fatalf("concurrent block = blocked %v err %v, want committed reviewing block", blocked, blockErr)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("external merges = %d, want none after reviewing state changed", len(gh.merges))
	}
	task, taskErr := store.GetTask(ctx, "task-9")
	if taskErr != nil || task.State != string(TaskBlocked) {
		t.Fatalf("task=%+v err=%v, want blocked", task, taskErr)
	}
}

func TestPolicyMergeGateRejectsBlockAfterInitialReadyValidation(t *testing.T) {
	ctx := context.Background()
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-job", agent: "audit", decision: "approved", hasResult: true,
	})
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	matched, current, err := store.RevalidateTaskState(ctx, "task-9", string(TaskReadyToMerge))
	if err != nil || !matched || current != string(TaskReadyToMerge) {
		t.Fatalf("initial ready validation = matched %v current %q err %v", matched, current, err)
	}
	request.Reviewer = "audit"
	request.ExpectedTaskState = string(TaskReadyToMerge)
	var blocked bool
	var blockErr error
	gate.Git.(*fakeMergeGateGit).onClean = func() {
		blocked, blockErr = store.BlockTaskWithEvent(ctx, db.Task{
			ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
			State: string(TaskBlocked),
		}, db.TaskEvent{
			Kind: "workflow_blocked", FromState: string(TaskReadyToMerge),
			Reason: "production block after initial validation",
		})
	}

	decision, err := gate.Evaluate(ctx, request)
	if !errors.Is(err, ErrMergeTaskStateChanged) {
		t.Fatalf("Evaluate error = %v, want ready-state fence conflict", err)
	}
	if !blocked || blockErr != nil {
		t.Fatalf("production block = blocked %v err %v, want committed block", blocked, blockErr)
	}
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("decision=%+v merges=%d, external merge must not run after block", decision, len(gh.merges))
	}
	task, taskErr := store.GetTask(ctx, "task-9")
	if taskErr != nil || task.State != string(TaskBlocked) {
		t.Fatalf("task=%+v err=%v, want blocked", task, taskErr)
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
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
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

	_, err := gate.Evaluate(ctx, MergeRequest{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
		ExpectedTaskState: string(TaskReadyToMerge),
	})

	if !errors.Is(err, mergeErr) {
		t.Fatalf("Evaluate error = %v, want %v", err, mergeErr)
	}
	if hasStatus(gh.statuses, GitmootMergeGateContext, "success") {
		t.Fatalf("statuses after failed merge = %+v, must not contain merge-gate success", gh.statuses)
	}
	blocked, blockErr := store.BlockTaskWithEvent(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskBlocked),
	}, db.TaskEvent{
		Kind: "workflow_blocked", FromState: string(TaskReadyToMerge),
		Reason: "merge failure remained unresolved",
	})
	if blockErr == nil || blocked || !strings.Contains(blockErr.Error(), "claimed for an external merge") {
		t.Fatalf("post-failure block = blocked %v err %v, want retained unresolved merge claim", blocked, blockErr)
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
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
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

	decision, err := gate.Evaluate(ctx, MergeRequest{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
		ExpectedTaskState: string(TaskReadyToMerge),
	})

	if err != nil {
		t.Fatalf("Evaluate returned error after completed merge: %v", err)
	}
	if !decision.Merged || decision.MergeCommitSHA != "merge123" {
		t.Fatalf("decision = %+v, want completed merge despite status error", decision)
	}
	if got := strings.Join(gh.operations, ","); got != "merge,status:gitmoot/merge-gate:success" {
		t.Fatalf("GitHub write order = %q, want merge before best-effort success status", got)
	}
	task, taskErr := store.GetTask(ctx, "task-9")
	if taskErr != nil || task.State != string(TaskMerged) {
		t.Fatalf("task=%+v err=%v, want durable claim completion", task, taskErr)
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
			name:          "no implement job",
			reviewerAgent: "audit",
			wantReason:    "no implement job is recorded for this task",
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
			if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Ready || decision.Merged {
				t.Fatalf("decision = %+v, want escalating LeaveOpen", decision)
			}
			if !strings.Contains(decision.Reason.Render(), tc.wantReason) {
				t.Fatalf("decision reason = %q, want %q", decision.Reason, tc.wantReason)
			}
			if len(gh.merges) != 0 {
				t.Fatalf("unverifiable approval issued merge: %+v", gh.merges)
			}
		})
	}
}

// insertMergeGatePanelChild adds one delegation child under a head-bound parent
// review. The parent declares its delegations itself in these tests, so this
// deliberately does not touch the parent payload.
func insertMergeGatePanelChild(t *testing.T, store *db.Store, parentID, delegationID string, state JobState, result *AgentResult) {
	t.Helper()
	encoded, err := marshalPayload(JobPayload{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Result: result,
	})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID:           parentID + "/delegation/" + delegationID,
		Agent:        delegationID,
		Type:         "review",
		State:        string(state),
		Payload:      encoded,
		ParentJobID:  parentID,
		DelegationID: delegationID,
	}, db.JobEvent{Kind: string(state), Message: "panel child"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
}

func insertMergeGateFanOutRow(t *testing.T, store *db.Store, decision string) {
	t.Helper()
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-panel", Agent: "g6-review-sol", Type: "review"}, JobPayload{
		Repo: "mobile/app", Branch: "task-9", PullRequest: 9, HeadSHA: "head123",
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: decision,
			Summary:  "Convening a three-reviewer panel at exact head head123",
			Delegations: []Delegation{
				{ID: "lens-a", Agent: "r1", Action: "review"},
				{ID: "lens-b", Agent: "r2", Action: "review"},
				{ID: "lens-c", Agent: "r3", Action: "review"},
			},
		},
	})
}

// #1685. A fan-out row is an announcement, so the delegates it named — not its
// own decision — decide the slot. This is the flow the shipped review-panel
// template produces, and the first version of this guard refused it outright.
func TestPolicyMergeGateMergesDelegatedReviewWhenPanelReported(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertMergeGateFanOutRow(t, store, "approved")
	for _, lens := range []string{"lens-a", "lens-b", "lens-c"} {
		insertMergeGatePanelChild(t, store, "review-panel", lens, JobSucceeded, &AgentResult{
			Decision: "approved", Summary: "lens verified the head", TestsRun: []string{"go test ./... -> ok"},
		})
	}

	if err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "g6-review-sol",
	}, "head123"); err != nil {
		t.Fatalf("a reported panel must clear the gate, got %v", err)
	}
}

// A panel that was announced and never dispatched carries no evidence. It must
// not satisfy the gate — that is the #1685 defect — and it must not be reported
// as a generic "review is not captured" either.
func TestPolicyMergeGateRefusesUndispatchedFanOutRow(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertMergeGateFanOutRow(t, store, "approved")

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "g6-review-sol",
	}, "head123")
	if err == nil {
		t.Fatal("an undispatched fan-out must not satisfy the gate")
	}
	// Not mergeBlocked: a fan-out is a missing verdict, not a quality rejection,
	// and classifying it as one both misreports the cause and parks the task.
	var blocked mergeBlocked
	if errors.As(err, &blocked) {
		t.Fatalf("undispatched fan-out returned mergeBlocked %q, want a missing-verdict error", blocked.reason)
	}
	for _, want := range []string{"g6-review-sol", "review-panel", "not a verdict"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must name %q", err, want)
		}
	}
}

// The wedge finding: blocking on the fan-out row made the head unrecoverable,
// because supersession is same-agent-only and a coordinator's continuation is
// dispatched as an "ask". A skipped row lets a real verdict at the SAME head
// decide, with no new commit required.
func TestPolicyMergeGateLetsIndependentVerdictClearFanOutRow(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertMergeGateFanOutRow(t, store, "approved")
	insertCompletedJob(t, store, db.Job{ID: "review-independent", Agent: "reviewer-y", Type: "review"}, JobPayload{
		Repo: "mobile/app", Branch: "task-9", PullRequest: 9, HeadSHA: "head123",
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "read the diff at head123",
			TestsRun: []string{"go test ./... -> ok"},
		},
	})

	if err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "reviewer-y",
	}, "head123"); err != nil {
		t.Fatalf("an independent verdict at the same head must clear a fan-out row, got %v", err)
	}
}

// Partial dispatch: children exist, so the panel did run, but one declared
// delegate produced no row. Its evidence is outstanding, not absent, so the gate
// waits instead of merging on the two that answered.
func TestPolicyMergeGateWaitsForDeclaredDelegationWithoutChild(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertMergeGateFanOutRow(t, store, "approved")
	for _, lens := range []string{"lens-a", "lens-b"} {
		insertMergeGatePanelChild(t, store, "review-panel", lens, JobSucceeded, &AgentResult{
			Decision: "approved", Summary: "lens verified the head",
		})
	}

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "g6-review-sol",
	}, "head123")
	var pending mergePending
	if !errors.As(err, &pending) {
		t.Fatalf("ensureFinalReviewCaptured = %v, want mergePending for an unreported delegate", err)
	}
	if !strings.Contains(pending.reason, "lens-c") {
		t.Fatalf("pending reason %q must name the delegate that never reported", pending.reason)
	}
}

// blocked/failed rows are not announcements: they already refuse on their own
// terms, and reclassifying them as fan-outs reported the wrong cause for a row
// nobody could mistake for an approval.
func TestPolicyMergeGateReportsBlockingCauseForDelegatingBlockedRow(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertMergeGateFanOutRow(t, store, "blocked")

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "g6-review-sol",
	}, "head123")
	var blocked mergeBlocked
	if !errors.As(err, &blocked) {
		t.Fatalf("ensureFinalReviewCaptured = %v, want mergeBlocked for a blocked row", err)
	}
	if !strings.Contains(blocked.reason, "blocking result from g6-review-sol") {
		t.Fatalf("reason %q must report the blocking verdict, not a fan-out diagnosis", blocked.reason)
	}
}

// A fan-out that announced changes_requested is still only an ANNOUNCEMENT, so
// it must not veto the head either. This is the direction that separates
// "excluded from the verdict population" from "cannot satisfy the gate": an
// approved fan-out is already non-blocking because approved is not a blocking
// decision, so only a changes_requested one can prove the exclusion is real.
//
// The distinction from TestPolicyMergeGateReportsBlockingCauseForDelegatingBlockedRow
// is deliberate: blocked/failed are leaf refusals that keep their own cause,
// while approved/changes_requested from a delegating row are continuations.
func TestPolicyMergeGateDoesNotBlockOnAChangesRequestedFanOut(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertMergeGateFanOutRow(t, store, "changes_requested")
	insertCompletedJob(t, store, db.Job{ID: "review-independent", Agent: "reviewer-y", Type: "review"}, JobPayload{
		Repo: "mobile/app", Branch: "task-9", PullRequest: 9, HeadSHA: "head123",
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "read the diff at head123",
			TestsRun: []string{"go test ./... -> ok"},
		},
	})

	if err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "reviewer-y",
	}, "head123"); err != nil {
		t.Fatalf("a changes_requested fan-out must not veto an independent verdict, got %v", err)
	}
}

// ACCEPTANCE: the same gate must still clear a real approval. A guard that also
// blocks honest verdicts is one that gets switched off.
func TestPolicyMergeGateClearsReviewVerdictWithoutDelegations(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-real", Agent: "g7-review", Type: "review"}, JobPayload{
		Repo: "mobile/app", Branch: "task-9", PullRequest: 9, HeadSHA: "head123",
		TaskID: "task-9", ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "approved", Summary: "verified at exact head",
			TestsRun: []string{"go test ./... -> ok"},
		},
	})
	if err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "g7-review",
	}, "head123"); err != nil {
		t.Fatalf("a real delegation-free approval must clear the gate, got %v", err)
	}
}

func TestPolicyMergeGateTreatsSubthresholdReviewAsIndependentApproval(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-notes", Agent: "audit", Type: "review"}, JobPayload{
		Repo:        "mobile/app",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested",
			Severity: reviewseverity.P2,
			Summary:  "non-blocking polish",
		},
	})

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "audit",
		ReviewBlockingSeverity: reviewseverity.P1,
	}, "head123")
	if err != nil {
		t.Fatalf("ensureFinalReviewCaptured returned error: %v", err)
	}
}

// A pipeline review stage binds its job to the task/PR/head under Sender=pipeline
// (internal/pipeline/run.go), so it lands in the merge gate's reviewsAtHead beside
// native reviews. AdvanceJob, the job.finished event and the PR comment renderer
// all leave a pipeline verdict raw; the gate is the surface with merge authority
// and must not be the one place that re-interprets it into an approval the
// pipeline never gave.
func TestPolicyMergeGateKeepsPipelineReviewVerdictRaw(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	base := JobPayload{
		Repo: "mobile/app", Branch: "task-9", PullRequest: 9, HeadSHA: "head123",
		TaskID: "task-9", ReviewRound: "review-1",
	}
	nativeApproval := base
	nativeApproval.Result = &AgentResult{Decision: "approved", Summary: "ok"}
	insertIndependentMergeGateReview(t, store, db.Job{ID: "review-native", Agent: "audit", Type: "review"}, nativeApproval)

	pipelineReview := base
	pipelineReview.Sender = PipelineJobSender
	pipelineReview.Result = &AgentResult{
		Decision: "changes_requested", Severity: reviewseverity.P2, Summary: "stage refused",
	}
	insertCompletedJob(t, store, db.Job{ID: "review-pipeline", Agent: "stagebot", Type: "review"}, pipelineReview)

	for _, threshold := range []string{reviewseverity.P3, reviewseverity.P1} {
		err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
			Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "audit",
			ReviewBlockingSeverity: threshold,
		}, "head123")
		var blocked mergeBlocked
		if !errors.As(err, &blocked) || !strings.Contains(blocked.reason, "stagebot") {
			t.Fatalf("threshold %s: ensureFinalReviewCaptured = %v, want block naming stagebot", threshold, err)
		}
	}
}

func TestPolicyMergeGatePassesBlockingSeverityToDelegatedReviewEvidence(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	parentPayload := JobPayload{
		Repo:        "mobile/app",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "parent approved"},
	}
	insertIndependentMergeGateReview(t, store, db.Job{
		ID: "parent-review", Agent: "audit", Type: "review",
	}, parentPayload)
	insertCompletedJob(t, store, db.Job{
		ID:           "delegated-review",
		Agent:        "specialist",
		Type:         "review",
		ParentJobID:  "parent-review",
		DelegationID: "specialist-review",
	}, JobPayload{
		Repo:        "mobile/app",
		PullRequest: 9,
		TaskID:      "task-9",
		Result: &AgentResult{
			Decision: "changes_requested",
			Severity: reviewseverity.P2,
			Summary:  "non-blocking specialist note",
		},
	})

	gate := PolicyMergeGate{Store: store}
	if err := gate.ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "audit",
		ReviewBlockingSeverity: reviewseverity.P1,
	}, "head123"); err != nil {
		t.Fatalf("sub-threshold delegated review blocked final review: %v", err)
	}
	if err := gate.ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "audit",
		ReviewBlockingSeverity: reviewseverity.P2,
	}, "head123"); err == nil || !strings.Contains(err.Error(), "blocking children") {
		t.Fatalf("at-threshold delegated review error = %v, want blocking child evidence", err)
	}
}

func TestPolicyMergeGateTreatsHeadlessSubthresholdReviewAsApproval(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "sol", Type: "implement"}, JobPayload{
		Repo: "mobile/app", PullRequest: 9, HeadSHA: "head123", TaskID: "task-9",
		Result: &AgentResult{Decision: "implemented", Summary: "implemented"},
	})
	insertCompletedJob(t, store, db.Job{ID: "review-integration-notes", Agent: "audit", Type: "review"}, JobPayload{
		Repo:         "mobile/app",
		PullRequest:  9,
		TaskID:       "task-9",
		ReviewRound:  "review-1",
		DelegationID: "integration-review",
		WorktreePath: "/tmp/integration-review",
		Result: &AgentResult{
			Decision: "changes_requested",
			Severity: reviewseverity.P2,
			Summary:  "headless integration notes",
		},
	})

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "audit",
		ReviewBlockingSeverity: reviewseverity.P1,
	}, "head123")
	if err != nil {
		t.Fatalf("headless sub-threshold ensureFinalReviewCaptured returned error: %v", err)
	}
}

func TestPolicyMergeGateChecksHeadlessSubthresholdReviewAuthorship(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "implement-headless-self", Agent: "sol", Type: "implement"}, JobPayload{
		Repo: "mobile/app", PullRequest: 9, HeadSHA: "head123", TaskID: "task-9",
		Result: &AgentResult{Decision: "implemented", Summary: "implemented"},
	})
	insertCompletedJob(t, store, db.Job{ID: "review-headless-self", Agent: "sol", Type: "review"}, JobPayload{
		Repo:         "mobile/app",
		PullRequest:  9,
		TaskID:       "task-9",
		ReviewRound:  "review-1",
		DelegationID: "integration-review",
		WorktreePath: "/tmp/integration-review",
		Result: &AgentResult{
			Decision: "changes_requested",
			Severity: reviewseverity.P2,
			Summary:  "self-authored integration notes",
		},
	})

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "sol",
		ReviewBlockingSeverity: reviewseverity.P1,
	}, "head123")
	if err == nil || !strings.Contains(err.Error(), "independent reviewer is required") {
		t.Fatalf("headless self-authored sub-threshold approval error = %v, want independence failure", err)
	}
}

// The pre-round fallback (reached when no review is recorded at the evaluated
// head) also reads stored review payloads, so it must go through the same
// payload-aware authority as the at-head path. A pipeline stage verdict for an
// earlier head is not an approval this gate may claim: folding it made the gate
// report a self-approval authorship failure — i.e. it had already classified the
// stage's changes_requested as an approval — instead of the real reason the
// review is unusable. Sender is the only difference between the two cases.
func TestPolicyMergeGateFallbackKeepsPipelineReviewVerdictRaw(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sender string
		want   string
	}{
		{name: "native", sender: "", want: "independent reviewer is required"},
		{name: "pipeline", sender: PipelineJobSender, want: "different head SHA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			insertCompletedJob(t, store, db.Job{ID: "implement-stale", Agent: "stagebot", Type: "implement"}, JobPayload{
				Repo: "mobile/app", PullRequest: 9, HeadSHA: "head123", TaskID: "task-9",
				Result: &AgentResult{Decision: "implemented", Summary: "implemented"},
			})
			insertCompletedJob(t, store, db.Job{ID: "review-stale", Agent: "stagebot", Type: "review"}, JobPayload{
				Repo:        "mobile/app",
				PullRequest: 9,
				HeadSHA:     "oldhead",
				TaskID:      "task-9",
				Sender:      tc.sender,
				Result: &AgentResult{
					Decision: "changes_requested",
					Severity: reviewseverity.P2,
					Summary:  "stage refused",
				},
			})

			err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
				Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "stagebot",
				ReviewBlockingSeverity: reviewseverity.P1,
			}, "head123")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ensureFinalReviewCaptured error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPolicyMergeGatePassesBlockingSeverityToHeadlessDelegatedEvidence(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "implement-headless-delegated", Agent: "sol", Type: "implement"}, JobPayload{
		Repo: "mobile/app", PullRequest: 9, HeadSHA: "head123", TaskID: "task-9",
		Result: &AgentResult{Decision: "implemented", Summary: "implemented"},
	})
	insertCompletedJob(t, store, db.Job{ID: "review-headless-parent", Agent: "audit", Type: "review"}, JobPayload{
		Repo:         "mobile/app",
		PullRequest:  9,
		TaskID:       "task-9",
		ReviewRound:  "review-1",
		DelegationID: "integration-review",
		WorktreePath: "/tmp/integration-review",
		Result:       &AgentResult{Decision: "approved", Summary: "parent approved"},
	})
	insertCompletedJob(t, store, db.Job{
		ID:           "review-headless-child",
		Agent:        "specialist",
		Type:         "review",
		ParentJobID:  "review-headless-parent",
		DelegationID: "specialist-review",
	}, JobPayload{
		Repo:        "mobile/app",
		PullRequest: 9,
		TaskID:      "task-9",
		Result: &AgentResult{
			Decision: "changes_requested",
			Severity: reviewseverity.P2,
			Summary:  "non-blocking specialist note",
		},
	})

	gate := PolicyMergeGate{Store: store}
	if err := gate.ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "audit",
		ReviewBlockingSeverity: reviewseverity.P1,
	}, "head123"); err != nil {
		t.Fatalf("sub-threshold headless delegated review blocked final review: %v", err)
	}
	if err := gate.ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "audit",
		ReviewBlockingSeverity: reviewseverity.P2,
	}, "head123"); err == nil || !strings.Contains(err.Error(), "blocking children") {
		t.Fatalf("at-threshold headless delegated review error = %v, want blocking child evidence", err)
	}
}

func TestPolicyMergeGateChecksAuthorshipForSubthresholdApproval(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	payload := JobPayload{
		Repo:        "mobile/app",
		Branch:      "task-9",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
	}
	implementPayload := payload
	implementPayload.ReviewRound = ""
	implementPayload.Result = &AgentResult{Decision: "implemented", Summary: "implemented"}
	insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "sol", Type: "implement"}, implementPayload)
	reviewPayload := payload
	reviewPayload.Result = &AgentResult{
		Decision: "changes_requested",
		Severity: reviewseverity.P2,
		Summary:  "self-authored notes",
	}
	insertCompletedJob(t, store, db.Job{ID: "review-notes", Agent: "sol", Type: "review"}, reviewPayload)

	err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "sol",
		ReviewBlockingSeverity: reviewseverity.P1,
	}, "head123")
	if err == nil || !strings.Contains(err.Error(), "independent reviewer is required") {
		t.Fatalf("self-authored sub-threshold approval error = %v, want independence failure", err)
	}
}

func TestDelegatedReviewEvidenceUsesBlockingSeverity(t *testing.T) {
	childPayload := JobPayload{Result: &AgentResult{
		Decision: "changes_requested",
		Severity: reviewseverity.P2,
		Summary:  "delegated notes",
	}}
	encoded, err := marshalPayload(childPayload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	children := []db.Job{{
		ID: "child-review", Type: "review", State: string(JobSucceeded), Payload: string(encoded),
	}}

	if err := ensureDelegatedReviewEvidence(db.Job{ID: "parent-review"}, children, nil, reviewseverity.P1); err != nil {
		t.Fatalf("sub-threshold delegated review returned error: %v", err)
	}
	if err := ensureDelegatedReviewEvidence(db.Job{ID: "parent-review"}, children, nil, reviewseverity.P2); err == nil || !strings.Contains(err.Error(), "blocking") {
		t.Fatalf("at-threshold delegated review error = %v, want blocking evidence", err)
	}
	askChildren := []db.Job{{
		ID: "child-ask", Type: "ask", State: string(JobSucceeded), Payload: string(encoded),
	}}
	if err := ensureDelegatedReviewEvidence(db.Job{ID: "parent-review"}, askChildren, nil, reviewseverity.P1); err == nil || !strings.Contains(err.Error(), "blocking") {
		t.Fatalf("sub-threshold non-review child error = %v, want raw blocking decision", err)
	}
}

func TestPolicyMergeGateNamesImplementerAttributionDeclineCause(t *testing.T) {
	type seedImplementJobs func(*testing.T, *db.Store, JobPayload)
	sites := []struct {
		name       string
		reviewHead string
	}{
		{name: "current_head", reviewHead: "head123"},
		{name: "legacy_round", reviewHead: "old123"},
	}
	causes := []struct {
		name               string
		seed               seedImplementJobs
		omitUnrelatedJob   bool
		bridgePrecondition bool
		want               []string
		doNotWant          []string
	}{
		{
			name:               "zero_implement_jobs_anywhere",
			omitUnrelatedJob:   true,
			bridgePrecondition: true,
			want: []string{
				"no implement job is recorded for this task",
				"NOT disqualified",
				"attribution gap, not a failed independence check",
				"gitmoot job record --agent <implementing-agent>",
				"--type implement",
			},
			doNotWant: []string{"implemented in a pane"},
		},
		{
			name:               "no_implement_job",
			bridgePrecondition: true,
			want: []string{
				"no implement job is recorded for this task",
				"NOT disqualified",
				"attribution gap, not a failed independence check",
				"gitmoot job record --agent <implementing-agent>",
				"--type implement",
			},
			doNotWant: []string{"implemented in a pane"},
		},
		{
			name: "empty_implement_agent",
			seed: func(t *testing.T, store *db.Store, payload JobPayload) {
				insertCompletedJob(t, store, db.Job{ID: "implement-empty-agent", Type: "implement"}, payload)
			},
			want: []string{"matches this task but has no recorded agent", "attribution data anomaly"},
		},
		{
			name: "malformed_implement_payload",
			seed: func(t *testing.T, store *db.Store, _ JobPayload) {
				if err := store.CreateJobWithEvent(context.Background(), db.Job{
					ID: "implement-malformed", Agent: "implementer", Type: "implement", State: string(JobSucceeded), Payload: "{",
				}, db.JobEvent{Kind: string(JobSucceeded), Message: "done"}); err != nil {
					t.Fatalf("CreateJobWithEvent returned error: %v", err)
				}
			},
			want: []string{"implement job has a malformed payload", "corrupt-record anomaly"},
		},
	}

	for _, site := range sites {
		t.Run(site.name, func(t *testing.T) {
			for _, cause := range causes {
				t.Run(cause.name, func(t *testing.T) {
					ctx := context.Background()
					store := openEngineStore(t)
					basePayload := JobPayload{
						Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
						Result: &AgentResult{Decision: "implemented", Summary: "implemented"},
					}
					if !cause.omitUnrelatedJob {
						unrelatedPayload := basePayload
						unrelatedPayload.Repo = "other/repo"
						unrelatedPayload.PullRequest = 99
						unrelatedPayload.TaskID = "other-task"
						insertCompletedJob(t, store, db.Job{ID: "unrelated-implement", Agent: "other", Type: "implement"}, unrelatedPayload)
					}
					if cause.seed != nil {
						cause.seed(t, store, basePayload)
					}
					reviewPayload := basePayload
					reviewPayload.HeadSHA = site.reviewHead
					reviewPayload.ReviewRound = "review-1"
					reviewPayload.Result = &AgentResult{Decision: "approved", Summary: "approved"}
					insertCompletedJob(t, store, db.Job{ID: "review-job", Agent: "audit", Type: "review"}, reviewPayload)

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
					if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Ready || decision.Merged {
						t.Fatalf("decision = %+v, want fail-closed escalating LeaveOpen", decision)
					}
					if len(gh.merges) != 0 {
						t.Fatalf("unverifiable approval issued merge: %+v", gh.merges)
					}
					for _, want := range cause.want {
						if !strings.Contains(decision.Reason.Render(), want) {
							t.Errorf("decision reason = %q, want named evidence/remedy %q", decision.Reason, want)
						}
					}
					for _, doNotWant := range cause.doNotWant {
						if strings.Contains(decision.Reason.Render(), doNotWant) {
							t.Errorf("decision reason inferred unobserved workflow %q: %q", doNotWant, decision.Reason)
						}
					}
					if cause.bridgePrecondition {
						assertAttributionGapRemedy(t, decision.Reason.Render())
						assertRenderedAttributionGapDecline(t, decision.Reason.Render(), "head123")
					}
				})
			}
		})
	}
}

// boundAttributionGapRemedy is the clause the attribution-gap decline must carry verbatim:
// the runnable remedy, bound to the row that actually satisfies collectImplementerAttribution.
//
// This replaced a clause pinning a COORDINATOR BRIDGE procedure (#1765). The bridge text was
// the defect: it told an operator to merge on human judgment when the engine's own remedy --
// recording a durable implement row -- was already available and unnamed. A guard that pins
// the wrong remedy verbatim keeps it shipped, which is why this constant moved rather than
// being deleted.
const boundAttributionGapRemedy = "gitmoot job record --agent <implementing-agent> --repo <owner/repo> --type implement --decision implemented --task <task-id> --pr <number> --head-sha <sha>"

// forbiddenAttributionGapPermissions are phrasings that turn the decline into permission to
// merge without independence. The gate still REFUSES on an attribution gap; only the remedy
// changed. Any of these appearing in the reason means the refusal has been inverted.
var forbiddenAttributionGapPermissions = []string{
	"may bridge",
	"bridge anyway",
	"merge the lane",
	"advisory",
	"after manual judgment",
}

// renderedAttributionGapDecline is the COMPLETE operator-facing text of the attribution-gap
// decline, wrapper included.
//
// Pinning the constant is not sufficient. The reason an operator actually reads is assembled
// AROUND the constant in reviewAndCIGateMiss -- "review gate: " + err + " for head " + sha --
// so text added at the renderer leaves noImplementJobAttributionReason byte-identical and
// every constant-level guard green while changing what the reader is told to do.
//
// This pins what is READ, not what is stored.
//
// DO NOT "improve" this by inlining the constant's text as a literal. Composing the expectation
// FROM noImplementJobAttributionReason is deliberate: it holds the WRAPPER fixed while letting
// the payload vary, which is exactly what makes this guard independent of the byte pin in
// TestImplementerAttributionAnomalyDeclinesRemainByteStable. Measured with production mutants in
// both directions:
//
//	append at the RENDERER  -> this guard FAILS (both cause paths); the byte pin passes
//	append inside the CONSTANT -> the byte pin FAILS; this guard passes (both sides move together)
func renderedAttributionGapDecline(headSHA string) string {
	return "review gate: " + noImplementJobAttributionReason + " for head " + headSHA
}

func assertRenderedAttributionGapDecline(t *testing.T, reason, headSHA string) {
	t.Helper()
	if want := renderedAttributionGapDecline(headSHA); reason != want {
		t.Fatalf("rendered operator-facing decline =\n  %q\nwant byte-identical\n  %q", reason, want)
	}
}

// attributionGapRemedyError reports why an attribution-gap decline fails to hand the lane a
// runnable remedy, or nil when it does. Three properties, each independently mutable:
//
//  1. it names the runnable row verbatim (boundAttributionGapRemedy);
//  2. it says the approval is NOT disqualified, so the reader does not misdiagnose an
//     attribution gap as a failed independence check -- the #1765 conflation;
//  3. it grants no permission to merge without independence.
//
// It is a function returning an error rather than a t.Fatalf helper so the guard itself can be
// mutation-tested: TestAttributionGapRemedyRejectsSemanticInversion feeds it texts that must be
// rejected. A guard that cannot be shown to fail is not a guard.
func attributionGapRemedyError(reason string) error {
	lower := strings.ToLower(reason)
	if !strings.Contains(lower, strings.ToLower(boundAttributionGapRemedy)) {
		return fmt.Errorf("attribution-gap decline must name the runnable remedy verbatim (%q): %q",
			boundAttributionGapRemedy, reason)
	}
	if !strings.Contains(lower, "not disqualified") {
		return fmt.Errorf("attribution-gap decline must state the approval is NOT disqualified, or a reader misreads a missing row as a failed independence check: %q", reason)
	}
	for _, forbidden := range forbiddenAttributionGapPermissions {
		if strings.Contains(lower, forbidden) {
			return fmt.Errorf("attribution-gap decline must not authorize merging without independence (found %q): %q", forbidden, reason)
		}
	}
	return nil
}

func assertAttributionGapRemedy(t *testing.T, reason string) {
	t.Helper()
	if err := attributionGapRemedyError(reason); err != nil {
		t.Fatal(err)
	}
}

// TestAttributionGapRemedyRejectsSemanticInversion pins the guard against the texts that
// motivated it, including the exact shape that shipped before #1765: a decline that reports the
// attribution gap as a failed independence check and sends the operator to a human bridge.
func TestAttributionGapRemedyRejectsSemanticInversion(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		{
			// The pre-#1765 production text, verbatim. It must now be REJECTED: it never
			// names the runnable row and it ends in permission to merge the lane.
			name: "pre-1765 coordinator bridge text",
			reason: "latest review round's approval cannot be verified as independent: no implement job is recorded for this task. " +
				"Use the coordinator bridge only as follows: step 1, confirm an independent approval exists at this exact head; " +
				"if it does not, do not bridge. If it does, read the engine review job's agent identity and decision at that head " +
				"with gitmoot job show <job-id>, confirm the implementer identity from the pane session, journal both with " +
				"gitmoot workflow note, then merge the lane",
		},
		{
			// Remedy named, refusal inverted: the reader is handed the row AND told the
			// restriction is optional.
			name: "remedy named but refusal made advisory",
			reason: "latest review round's approval is NOT disqualified, but independence cannot be verified. Remedy: " +
				boundAttributionGapRemedy + ". This restriction is advisory, so a coordinator may bridge anyway after manual judgment",
		},
		{
			// The conflation itself: correct remedy, but the reader is still told the
			// independence check FAILED, which is what produced three escalations.
			name:   "remedy named but still reported as failed independence",
			reason: "latest review round's approval cannot be verified as independent: no implement job is recorded. Remedy: " + boundAttributionGapRemedy,
		},
		{
			// Remedy weakened to a prose gesture with no runnable row.
			name: "remedy not runnable",
			reason: "latest review round's approval is NOT disqualified, but independence cannot be verified: " +
				"record an implement job for this task and re-evaluate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := attributionGapRemedyError(tc.reason); err == nil {
				t.Fatalf("guard accepted a decline that does not hand the lane a runnable remedy: %q", tc.reason)
			}
		})
	}
}

// TestAttributionGapRemedyAcceptsShippedText is the positive half: the guard must still accept
// the text actually shipped, so the negative cases above cannot be satisfied by a guard that
// rejects everything.
func TestAttributionGapRemedyAcceptsShippedText(t *testing.T) {
	if err := attributionGapRemedyError(noImplementJobAttributionReason); err != nil {
		t.Fatalf("guard rejected the shipped attribution-gap remedy: %v", err)
	}
}

func TestImplementerAttributionAnomalyDeclinesRemainByteStable(t *testing.T) {
	wants := map[string]struct {
		got  string
		want string
	}{
		"empty agent": {
			got:  emptyImplementAgentAttributionReason,
			want: "latest review round's approval cannot be verified as independent: an implement job matches this task but has no recorded agent; this is an attribution data anomaly",
		},
		"malformed payload": {
			got:  malformedImplementPayloadAttributionReason,
			want: "latest review round's approval cannot be verified as independent: an implement job has a malformed payload, so attribution for this task cannot be verified; this is a corrupt-record anomaly",
		},
		// The no-implement-job reason is the only one of the four that hands the operator a
		// PROCEDURE, so it is the only one where ADDED text can change what the reader does.
		// It was the sibling missing from this policy, and that omission is what let a
		// follow-on override -- "this restriction is advisory; a coordinator may bridge
		// anyway after manual judgment" -- be appended while the bound-clause guard still
		// passed.
		//
		// This byte pin SUBSUMES the bound-clause helper: exact equality rejects every append,
		// inversion, deletion and rewording of the constant, and the helper catches no
		// production mutation this pin permits. The helper is kept as a diagnostic that names
		// WHICH property broke, not as a second layer -- see the note above
		// attributionGapRemedyError.
		//
		// The guard it does NOT subsume is renderedAttributionGapDecline, which pins the
		// text assembled AROUND this constant. Those two are genuinely independent, measured
		// with production mutants in both directions.
		"no implement job": {
			got: noImplementJobAttributionReason,
			// EXTENDED DELIBERATELY (#1718), and this pin is why it had to be deliberate.
			// The old text named only --agent, so for in-session role work the remedy it
			// printed was UNRUNNABLE: `job record --agent gitmoot` is refused, gitmoot
			// being an org role rather than one of the registered agents. A decline that
			// hands the operator a command which cannot succeed is a dead end wearing the
			// costume of a remedy. The added clause names --acting-role and states that
			// independence is still enforced, so the escape hatch cannot be read as a
			// waiver.
			want: "latest review round's approval is NOT disqualified, but independence cannot be verified: no implement job is recorded for this task, so the gate cannot establish who implemented it. This is an attribution gap, not a failed independence check, and it is the expected state for in-session implementation. Remedy, runnable by the implementing lane: record the durable attribution row with gitmoot job record --agent <implementing-agent> --repo <owner/repo> --type implement --decision implemented --task <task-id> --pr <number> --head-sha <sha>, then re-evaluate. Do not record an agent that did not implement, and do not record the reviewer. If the work was done in session by an org role that is not a registered agent, record --acting-role <role> in place of --agent: attribution is then the role, and independence is still enforced against it",
		},
	}
	for name, check := range wants {
		t.Run(name, func(t *testing.T) {
			if check.got != check.want {
				t.Fatalf("decline = %q, want byte-identical %q", check.got, check.want)
			}
		})
	}
}

func TestPolicyMergeGateEmptyReviewRoundUsesRecordedRecency(t *testing.T) {
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
	insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "implementer", Type: "implement"}, implementPayload)

	olderReview := basePayload
	olderReview.HeadSHA = "older-head"
	olderReview.Result = &AgentResult{Decision: "approved", Summary: "older approval"}
	const olderJobID = "local-review-zulu-older"
	insertCompletedJob(t, store, db.Job{ID: olderJobID, Agent: "zulu-reviewer", Type: "review"}, olderReview)
	setMergeGateJobTimestamps(t, store, olderJobID, "2026-08-31 10:00:00")

	newerReview := basePayload
	newerReview.HeadSHA = "newer-head"
	newerReview.Result = &AgentResult{Decision: "approved", Summary: "newer approval"}
	const newerJobID = "local-review-alpha-newer"
	insertCompletedJob(t, store, db.Job{ID: newerJobID, Agent: "alpha-reviewer", Type: "review"}, newerReview)
	setMergeGateJobTimestamps(t, store, newerJobID, "2026-08-31 11:00:00")

	insertMergeGateDelegationChild(t, store, olderJobID, "lens-latest", JobFailed, nil)
	setMergeGateJobTimestamps(t, store, olderJobID+"/delegation/lens-latest", "2026-08-31 12:00:00")

	mergeable := true
	gh := &fakeMergeGateGitHub{
		pr: github.PullRequest{
			Number: 9, State: "open", HeadRef: "task-9", BaseRef: "main",
			HeadSHA: "current-head", Mergeable: &mergeable,
		},
		status: github.CombinedStatus{State: "success", Statuses: []github.CommitStatus{{Context: "ci", State: "success"}}},
		checks: []github.PullRequestCheck{{Name: "ci", Bucket: "pass", State: "SUCCESS"}},
	}
	gate := PolicyMergeGate{AutoMerge: true, Store: store, GitHub: gh, Git: &fakeMergeGateGit{clean: true}}

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Ready || decision.Merged {
		t.Fatalf("decision = %+v, want escalating LeaveOpen", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "latest review from alpha-reviewer is for a different head SHA") {
		t.Fatalf("decision reason = %q, want newer root review selected by recorded time", decision.Reason)
	}
}

func TestPolicyMergeGateExplicitRoundPrecedesNewerSameReviewerVerdict(t *testing.T) {
	for _, tc := range []struct {
		name       string
		newerRound string
	}{
		{name: "empty round", newerRound: ""},
		{name: "lower explicit round", newerRound: "review-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "implementer", Type: "implement"}, implementPayload)

			blockingReview := basePayload
			blockingReview.HeadSHA = "head123"
			blockingReview.ReviewRound = "review-2"
			blockingReview.Result = &AgentResult{Decision: "changes_requested", Summary: "blocking"}
			insertCompletedJob(t, store, db.Job{ID: "review-blocking", Agent: "audit", Type: "review"}, blockingReview)
			setMergeGateJobTimestamps(t, store, "review-blocking", "2026-08-31 10:00:00")

			newerApproval := basePayload
			newerApproval.HeadSHA = "head123"
			newerApproval.ReviewRound = tc.newerRound
			newerApproval.Result = &AgentResult{Decision: "approved", Summary: "lower round approval"}
			insertCompletedJob(t, store, db.Job{ID: "review-newer", Agent: "audit", Type: "review"}, newerApproval)
			setMergeGateJobTimestamps(t, store, "review-newer", "2026-08-31 11:00:00")

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
			if !decision.LeaveOpen || decision.Merged {
				t.Fatalf("decision = %+v, want blocking review to keep PR open", decision)
			}
			if !strings.Contains(decision.Reason.Render(), "blocking result from audit") {
				t.Fatalf("decision reason = %q, want higher explicit round's blocking verdict", decision.Reason)
			}
		})
	}
}

func TestPolicyMergeGateNewerManualBlockingReviewSurvivesExplicitApproval(t *testing.T) {
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
	insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "implementer", Type: "implement"}, implementPayload)

	explicitApproval := basePayload
	explicitApproval.HeadSHA = "head123"
	explicitApproval.ReviewRound = "review-2"
	explicitApproval.Result = &AgentResult{Decision: "approved", Summary: "older approval"}
	insertCompletedJob(t, store, db.Job{ID: "review-explicit", Agent: "audit", Type: "review"}, explicitApproval)
	setMergeGateJobTimestamps(t, store, "review-explicit", "2026-08-31 10:00:00")

	manualBlock := basePayload
	manualBlock.HeadSHA = "head123"
	manualBlock.Result = &AgentResult{Decision: "changes_requested", Summary: "newer manual block"}
	insertCompletedJob(t, store, db.Job{ID: "review-manual", Agent: "audit", Type: "review"}, manualBlock)
	setMergeGateJobTimestamps(t, store, "review-manual", "2026-08-31 11:00:00")

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
	if !decision.LeaveOpen || decision.Merged {
		t.Fatalf("decision = %+v, want newer manual block to keep PR open", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "blocking result from audit") {
		t.Fatalf("decision reason = %q, want manual blocking verdict", decision.Reason)
	}
}

func TestPolicyMergeGateBlockingDelegationChildUnderNonReviewParent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	basePayload := JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-9",
		PullRequest: 9,
		TaskID:      "task-9",
		HeadSHA:     "head123",
	}
	implementPayload := basePayload
	implementPayload.HeadSHA = ""
	implementPayload.Result = &AgentResult{Decision: "implemented", Summary: "implemented"}
	insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "implementer", Type: "implement"}, implementPayload)

	approvalPayload := basePayload
	approvalPayload.ReviewRound = "review-1"
	approvalPayload.Result = &AgentResult{Decision: "approved", Summary: "root approval"}
	insertCompletedJob(t, store, db.Job{ID: "review-approval", Agent: "audit", Type: "review"}, approvalPayload)

	parentPayload := basePayload
	parentPayload.Result = &AgentResult{Decision: "approved", Summary: "orchestration complete"}
	insertCompletedJob(t, store, db.Job{ID: "orchestrate-parent", Agent: "coordinator", Type: "ask"}, parentPayload)

	childPayload := basePayload
	childPayload.Result = &AgentResult{Decision: "changes_requested", Summary: "delegated blocker"}
	insertCompletedJob(t, store, db.Job{
		ID:           "orchestrate-parent/delegation/security",
		Agent:        "security",
		Type:         "review",
		ParentJobID:  "orchestrate-parent",
		DelegationID: "security",
	}, childPayload)

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
	if !decision.LeaveOpen || decision.Merged {
		t.Fatalf("decision = %+v, want delegated blocker to keep PR open", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "blocking result from security") {
		t.Fatalf("decision reason = %q, want delegated blocking verdict", decision.Reason)
	}
}

// TestPolicyMergeGateUndecidedRoundOrderNeverHidesUnfinishedReviewer pins the
// property that replaced per-reviewer "latest row wins" selection: when two rows
// from one reviewer at the evaluated head cannot be ordered (one explicit
// review-N, one CLI-dispatched empty round), an unfinished or crashed slot must
// still block. It is asserted in BOTH job-id orders, so an implementation that
// resolves the tie through ListJobs' ORDER BY id fails one direction, and the
// approval is recorded LATER than the unfinished row, so a "latest timestamp
// wins" implementation fails both.
func TestPolicyMergeGateUndecidedRoundOrderNeverHidesUnfinishedReviewer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       JobState
		wantReason  string
		wantPending bool
	}{
		{name: "queued", state: JobQueued, wantReason: "waiting for reviewer audit", wantPending: true},
		{name: "running", state: JobRunning, wantReason: "waiting for reviewer audit", wantPending: true},
		{name: "failed", state: JobFailed, wantReason: "crashed reviewer audit"},
		{name: "cancelled", state: JobCancelled, wantReason: "crashed reviewer audit"},
	} {
		for _, order := range []struct {
			name       string
			manualID   string
			numberedID string
		}{
			{name: "manual id sorts first", manualID: "aaa-manual-review", numberedID: "zzz-dispatched-review"},
			{name: "numbered id sorts first", manualID: "zzz-manual-review", numberedID: "aaa-dispatched-review"},
		} {
			t.Run(tc.name+"/"+order.name, func(t *testing.T) {
				store, gh, gate, request := newMergeGateQuorumScenario(t)
				insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
					id: order.numberedID, agent: "audit", state: tc.state,
					recorded: "2026-07-31 12:00:00",
				})
				insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
					id: order.manualID, agent: "audit", hasResult: true, decision: "approved",
					recorded: "2026-07-31 12:01:00", emptyRound: true,
				})

				decision, err := gate.Evaluate(context.Background(), request)
				if err != nil {
					t.Fatalf("Evaluate returned error: %v", err)
				}
				if decision.Merged {
					t.Fatalf("decision = %+v, want unfinished reviewer slot to hold the merge", decision)
				}
				if tc.wantPending && decision.Reason.IsGateMiss() {
					t.Fatalf("decision = %+v, want unfinished slot to wait without escalating", decision)
				}
				if !tc.wantPending && (!decision.LeaveOpen || !decision.Reason.IsGateMiss()) {
					t.Fatalf("decision = %+v, want crashed slot parked as a gate miss", decision)
				}
				if !strings.Contains(decision.Reason.Render(), tc.wantReason) ||
					!strings.Contains(decision.Reason.Render(), order.numberedID) {
					t.Fatalf("decision reason = %q, want %q naming %s", decision.Reason, tc.wantReason, order.numberedID)
				}
				if len(gh.merges) != 0 {
					t.Fatalf("merge calls = %+v, want none", gh.merges)
				}
			})
		}
	}
}

// TestPolicyMergeGateAdvancesIntegrationWorktreeReviewAsDelegationChild is the
// #388 regression in its PRODUCTION row shape: mailbox.Enqueue writes both
// ParentJobID and DelegationID, so the gate-required #332 integration review IS a
// delegation child. Its HeadSHA is cleared by the engine, so it can never appear
// in the exact-head scan; excluding it from round selection as a round-history
// duplicate deadlocks the merge. Its parent is an orchestrating job, not a review,
// so it is nobody's sub-review.
func TestPolicyMergeGateAdvancesIntegrationWorktreeReviewAsDelegationChild(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "gitmoot/gitmoot",
		Number:       9,
		HeadBranch:   "task-9",
		BaseBranch:   "main",
		HeadSHA:      "head123",
		State:        "open",
	}); err != nil {
		t.Fatalf("UpsertPullRequest returned error: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "coordinator-job", Agent: "coordinator", Type: "ask"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		TaskID:      "task-9",
		Result:      &AgentResult{Decision: "approved", Summary: "verify gate fanned out"},
	})
	insertIndependentMergeGateReview(t, store, db.Job{
		ID:           "coordinator-job/delegation/verify-gate",
		Agent:        "audit",
		Type:         "review",
		ParentJobID:  "coordinator-job",
		DelegationID: "verify-gate",
	}, JobPayload{
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
		t.Fatalf("production-shape integration review did not advance to merge: decision = %+v", decision)
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Ready || decision.Merged {
		t.Fatalf("decision = %+v, want escalating LeaveOpen", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "approval was authored by sol, the implementing agent") {
		t.Fatalf("decision reason lost self-approval cause: %q", decision.Reason)
	}
	if strings.Contains(decision.Reason.Render(), "different head SHA") {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Ready || decision.Merged {
		t.Fatalf("decision = %+v, want escalating LeaveOpen", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "approval was authored by sol, the implementing agent") {
		t.Fatalf("decision reason lost self-approval cause: %q", decision.Reason)
	}
	if strings.Contains(decision.Reason.Render(), "different head SHA") {
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
		if !decision.LeaveOpen || decision.Ready || decision.Merged || decision.Deferred || decision.Reason.Render() != MergeLeaveOpenAutoMergeKillSwitchReason {
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
				decision, err := engine.runMergeGate(ctx, "", payload, taskRef{ID: tc.taskID, Repo: "owner/repo", Title: tc.name, Branch: tc.taskID}, TaskReadyToMerge)
				if err != nil {
					t.Fatalf("runMergeGate attempt %d: %v", attempt+1, err)
				}
				if !decision.LeaveOpen || decision.Reason.Render() != MergeLeaveOpenAutoMergeKillSwitchReason {
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

func TestRunMergeGateDraftPullRequestDoesNotParkTaskAwaitingHumanMerge(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	const taskID = "task-draft"
	if err := store.UpsertTask(ctx, db.Task{
		ID: taskID, RepoFullName: "owner/repo", Title: "Draft task",
		State: string(TaskReadyToMerge), Branch: "task-draft",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	engine := Engine{Store: store, MergeGate: PolicyMergeGate{AutoMerge: true, GitHub: &fakeMergeGateGitHub{}}}
	payload := JobPayload{
		Repo: "owner/repo", Branch: "task-draft", PullRequest: 17,
		PullRequestDraft: true, TaskID: taskID, TaskTitle: "Draft task",
	}

	decision, err := engine.runMergeGate(ctx, "", payload, taskRef{
		ID: taskID, Repo: "owner/repo", Title: "Draft task", Branch: "task-draft",
	}, TaskReadyToMerge)
	if err != nil {
		t.Fatalf("runMergeGate: %v", err)
	}
	if !decision.LeaveOpen || decision.Reason.Render() != "pull request is draft" {
		t.Fatalf("decision = %+v, want draft leave-open", decision)
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.State != string(TaskReadyToMerge) {
		t.Fatalf("task state = %q, want %q", task.State, TaskReadyToMerge)
	}
	events, err := store.ListTaskEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == "task_awaiting_human_merge" {
			t.Fatalf("draft PR created a pending-human-decision park: %+v", events)
		}
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want escalating LeaveOpen", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "final agent review is not captured") {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want escalating LeaveOpen", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "external CI") {
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
	if !decision.Merged || decision.LeaveOpen || decision.Reason.IsGateMiss() {
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
	if !decision.Ready || !decision.Merged || decision.MergeCommitSHA != "merge123" || decision.Reason.Render() != "merged" {
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
	if !decision.Merged || decision.Reason.Render() != "merged" {
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
	if !decision.Merged || !strings.Contains(decision.Reason.Render(), "cleanup task worktree") {
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
	if !decision.Merged || !strings.Contains(decision.Reason.Render(), "task task-8 branch is task-8") {
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
	if decision.Ready || !strings.Contains(decision.Reason.Render(), "worktree") {
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
	if decision.Ready || !strings.Contains(decision.Reason.Render(), "external CI") {
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
	if !decision.LeaveOpen || !strings.Contains(decision.Reason.Render(), "not successful") {
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
	if !decision.Ready || decision.Merged || !strings.Contains(decision.Reason.Render(), "branch update") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if len(gh.updates) != 1 || gh.updates[0].ExpectedHeadSHA != "head123" {
		t.Fatalf("update inputs = %+v", gh.updates)
	}
	if !hasStatus(gh.statuses, GitmootMergeGateContext, "pending") {
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
	if decision.Ready || !strings.Contains(decision.Reason.Render(), "conflicts with main") {
		t.Fatalf("decision = %+v", decision)
	}
	if !hasStatus(gh.statuses, GitmootMergeGateContext, "failure") {
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
	if !decision.Ready || decision.Merged || !strings.Contains(decision.Reason.Render(), "head changed") {
		t.Fatalf("decision = %+v", decision)
	}
	if !hasStatus(gh.statuses, GitmootMergeGateContext, "pending") {
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
	if !decision.Ready || decision.Merged || !strings.Contains(decision.Reason.Render(), "merge queue") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if !hasStatus(gh.statuses, GitmootMergeGateContext, "pending") {
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
	if !decision.Ready || decision.Merged || !strings.Contains(decision.Reason.Render(), "pending") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if !hasStatus(gh.statuses, GitmootMergeGateContext, "pending") {
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
	if !decision.Ready || decision.Merged || !strings.Contains(decision.Reason.Render(), "pending") {
		t.Fatalf("decision = %+v", decision)
	}
	if _, err := store.GetBranchLock(ctx, "gitmoot/gitmoot", "task-9"); err != nil {
		t.Fatalf("branch lock after queued merge error = %v", err)
	}
	if _, err := store.GetPullRequest(ctx, "gitmoot/gitmoot", 9); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetPullRequest after queued merge error = %v, want sql.ErrNoRows", err)
	}
	if !hasStatus(gh.statuses, GitmootMergeGateContext, "pending") {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || !strings.Contains(decision.Reason.Render(), "final agent review is not captured") {
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
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, _, err := store.ClaimTaskState(ctx, "task-9", string(TaskReadyToMerge),
		"external_merge", time.Minute); err != nil || !claimed {
		t.Fatalf("seed crashed merge claim = claimed %v err %v", claimed, err)
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

	decision, err := gate.Evaluate(ctx, MergeRequest{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
		ExpectedTaskState: string(TaskReadyToMerge),
	})

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
	task, taskErr := store.GetTask(ctx, "task-9")
	if taskErr != nil || task.State != string(TaskMerged) {
		t.Fatalf("task=%+v err=%v, want recovered merged state", task, taskErr)
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
	if decision.Ready || !strings.Contains(decision.Reason.Render(), "closed") {
		t.Fatalf("decision = %+v", decision)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge inputs = %+v", gh.merges)
	}
	if !hasStatus(gh.statuses, GitmootMergeGateContext, "failure") {
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
			if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
				t.Fatalf("decision = %+v, want objection to block merge", decision)
			}
			if !strings.Contains(decision.Reason.Render(), "blocking result from objector") {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want different reviewer's objection to remain blocking", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "blocking result from objector") {
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
			if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
				t.Fatalf("decision = %+v, want non-evidence decision %q to leave objection blocking", decision, tc.decision)
			}
			if !strings.Contains(decision.Reason.Render(), "blocking result from objector") {
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
	if decision.Merged || decision.Reason.IsGateMiss() {
		t.Fatalf("decision = %+v, want queued review to wait without escalating", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "waiting for reviewer reviewer-b") ||
		!strings.Contains(decision.Reason.Render(), "review-queued") {
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
	if decision.Merged || decision.Reason.IsGateMiss() {
		t.Fatalf("decision = %+v, want running review to wait without escalating", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "waiting for reviewer reviewer-b") ||
		!strings.Contains(decision.Reason.Render(), "review-running") {
		t.Fatalf("decision reason = %q, want running reviewer and job", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

// TestPolicyMergeGateLatestQueuedReviewWaitsOverEarlierObjection uses the
// PRODUCTION requeue shape: `gitmoot job retry` re-runs the SAME row, nilling its
// result and returning it to queued, so an objection followed by a requeue is one
// row whose state changed -- not two rows sharing round review-1, which no
// dispatch path can create.
func TestPolicyMergeGateLatestQueuedReviewWaitsOverEarlierObjection(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-requeued", agent: "reviewer-a", hasResult: true, decision: "changes_requested",
		recorded: "2026-07-31 12:00:00",
	})
	settleMergeGateReviewInPlace(t, store, "review-requeued", JobQueued, nil, "2026-07-31 12:01:00")
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-other-approved", agent: "reviewer-b", hasResult: true, decision: "approved",
		recorded: "2026-07-31 12:02:00",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Merged || decision.Reason.IsGateMiss() {
		t.Fatalf("decision = %+v, want latest queued review to wait", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "waiting for reviewer reviewer-a") ||
		!strings.Contains(decision.Reason.Render(), "review-requeued") {
		t.Fatalf("decision reason = %q, want latest queued reviewer and job", decision.Reason)
	}
	if strings.Contains(decision.Reason.Render(), "blocking result") {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want failed reviewer parked", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "crashed reviewer joltra-sol-review") ||
		!strings.Contains(decision.Reason.Render(), "local-review-joltra-sol-review-18c723c7768aa61d") {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want cancelled reviewer parked", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "crashed reviewer reviewer-b") ||
		!strings.Contains(decision.Reason.Render(), "review-cancelled") {
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
			if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
				t.Fatalf("decision = %+v, want abstaining reviewer parked", decision)
			}
			if !strings.Contains(decision.Reason.Render(), "abstaining reviewer reviewer-b") ||
				!strings.Contains(decision.Reason.Render(), "review-abstained") {
				t.Fatalf("decision reason = %q, want abstaining reviewer and job", decision.Reason)
			}
			if strings.Contains(decision.Reason.Render(), "crashed reviewer") {
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
	// The two local-review-* rows are CLI dispatches: `gitmoot agent review` sets
	// the head and never a round, so in production they carry EMPTY rounds. Only
	// the workflow-* row is an engine round.
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id:    "local-review-joltra-sol-review-18c723c7768aa61d",
		agent: "joltra-sol-review", state: JobFailed, emptyRound: true,
		recorded: "2026-07-31 18:11:56",
	})
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id:    "local-review-joltra-sol-review-18c7245ac25c6980",
		agent: "joltra-sol-review", hasResult: true, decision: "changes_requested",
		emptyRound: true, recorded: "2026-07-31 18:22:29",
	})

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want latest changes_requested verdict to block", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "blocking result from joltra-sol-review") {
		t.Fatalf("decision reason = %q, want latest reviewer's blocking result", decision.Reason)
	}
	if strings.Contains(decision.Reason.Render(), "crashed reviewer") {
		t.Fatalf("decision reason = %q, stale failed job must not park latest slot", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

// TestPolicyMergeGateLatestCrashOverridesStaleApproval uses the PRODUCTION crash
// shape: one row that carried an approval and was then retried in place and
// crashed. `gitmoot job retry` keeps the id, head, round and created_at, nils the
// result and advances updated_at, so the reviewer's slot is a single row -- never
// an approval row plus a separate failed row sharing round review-1.
func TestPolicyMergeGateLatestCrashOverridesStaleApproval(t *testing.T) {
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-retried-crashed", agent: "reviewer-a",
		hasResult: true, decision: "approved", recorded: "2026-07-31 12:00:00",
	})
	settleMergeGateReviewInPlace(t, store, "review-retried-crashed", JobFailed, nil, "2026-07-31 12:01:00")

	decision, err := gate.Evaluate(context.Background(), request)

	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want crashed reviewer parked despite the row's earlier approval", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "crashed reviewer reviewer-a") ||
		!strings.Contains(decision.Reason.Render(), "review-retried-crashed") {
		t.Fatalf("decision reason = %q, want crashed reviewer and job", decision.Reason)
	}
	if len(gh.merges) != 0 {
		t.Fatalf("merge calls = %+v, want none", gh.merges)
	}
}

// TestPolicyMergeGateEmptyRoundSupersessionUsesVerdictRecency pins the ordering
// invariant for two CLI-dispatched rows of one reviewer at the evaluated head:
// recency is when the VERDICT was recorded (updated_at, then created_at), never
// dispatch time. `gitmoot job retry` re-runs a row in place, keeping created_at
// and bumping updated_at, so the EARLIEST-created row can hold the NEWEST verdict;
// ordering by created_at let a later-created but earlier-recorded approval discard
// a retried row's changes_requested and merge the PR.
//
// The second case is the control: with the approval genuinely recorded last,
// supersession must still work and the PR must merge, so the first case cannot
// pass by refusing every supersession.
func TestPolicyMergeGateEmptyRoundSupersessionUsesVerdictRecency(t *testing.T) {
	for _, tc := range []struct {
		name           string
		retriedUpdated string
		freshUpdated   string
		wantMerged     bool
	}{
		{
			name:           "retried row records the newer objection",
			retriedUpdated: "2026-07-31 10:40:00",
			freshUpdated:   "2026-07-31 10:20:00",
		},
		{
			name:           "fresh row records the newer approval",
			retriedUpdated: "2026-07-31 10:20:00",
			freshUpdated:   "2026-07-31 10:40:00",
			wantMerged:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			// Dispatched FIRST (oldest created_at) and later retried in place, so its
			// verdict is carried by updated_at.
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: "local-review-audit-retried", agent: "audit", emptyRound: true,
				hasResult: true, decision: "changes_requested",
				createdAt: "2026-07-31 10:00:00", updatedAt: tc.retriedUpdated,
			})
			// Dispatched SECOND (newer created_at) and ran once.
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: "local-review-audit-fresh", agent: "audit", emptyRound: true,
				hasResult: true, decision: "approved",
				createdAt: "2026-07-31 10:10:00", updatedAt: tc.freshUpdated,
			})

			decision, err := gate.Evaluate(context.Background(), request)
			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if tc.wantMerged {
				if !decision.Merged {
					t.Fatalf("decision = %+v, want the later-recorded approval to supersede", decision)
				}
				if len(gh.merges) != 1 {
					t.Fatalf("merge calls = %+v, want one", gh.merges)
				}
				return
			}
			if decision.Merged || !decision.LeaveOpen || !decision.Reason.IsGateMiss() {
				t.Fatalf("decision = %+v, want the later-recorded objection to hold the merge", decision)
			}
			if !strings.Contains(decision.Reason.Render(), "blocking result from audit") {
				t.Fatalf("decision reason = %q, want the retried row's objection", decision.Reason)
			}
			if len(gh.merges) != 0 {
				t.Fatalf("merge calls = %+v, want none", gh.merges)
			}
		})
	}
}

// TestPolicyMergeGateRequeueClearsCrashedReviewerPark pins the PRODUCTION requeue
// shape. `gitmoot job retry` transitions the SAME row in place and preserves its
// id, head and round, so a requeue is one row whose state changes -- never a
// second row sharing the first's explicit round, which no dispatch path can
// create. Pinning the two-row shape hid a regression that refused every
// cross-round supersession.
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
	if !parked.LeaveOpen || !parked.Reason.IsGateMiss() || parked.Merged {
		t.Fatalf("first decision = %+v, want crashed reviewer parked", parked)
	}
	if !strings.Contains(parked.Reason.Render(), "crashed reviewer reviewer-b") ||
		!strings.Contains(parked.Reason.Render(), "review-crashed") {
		t.Fatalf("first decision reason = %q, want crashed reviewer and job", parked.Reason)
	}

	settleMergeGateReviewInPlace(t, store, "review-crashed", JobSucceeded,
		&AgentResult{Decision: "approved", Summary: "requeued verdict"}, "2026-07-31 12:02:00")

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

// TestPolicyMergeGateCLIReviewResolvesOnlySettledNonVerdictEngineRound pins the
// exception that makes the mixed-round refusal survivable. The engine always
// stamps review-N; `gitmoot agent review <reviewer> --repo gitmoot/gitmoot
// --pr <N> --head-sha <H>` creates a second row with the head set and NO round.
// A later CLI verdict may resolve an engine round that SETTLED WITHOUT a verdict
// -- no result, or a decision like "skipped" -- because that row is inescapable
// otherwise: `gitmoot job retry` and `gitmoot job cancel` both refuse a succeeded
// job and a re-poll of the same head dispatches nothing. It must NOT resolve a
// live, crashed or blocking row: those exits mutate the row itself.
func TestPolicyMergeGateCLIReviewResolvesOnlySettledNonVerdictEngineRound(t *testing.T) {
	const engineJobID = "review-audit-task-9-review-1"
	const cliJobID = "local-review-audit-18d0f3d229cc7aa3"
	for _, tc := range []struct {
		name        string
		state       JobState
		hasResult   bool
		decision    string
		cliRecorded string
		wantMerged  bool
		wantPending bool
		wantReason  string
	}{
		{
			name: "unrecognized decision is resolved", hasResult: true, decision: "skipped",
			cliRecorded: "2026-07-31 12:02:00", wantMerged: true,
		},
		{
			name:        "missing result is resolved",
			cliRecorded: "2026-07-31 12:02:00", wantMerged: true,
		},
		{
			name: "unrecognized decision parks without a cli review", hasResult: true, decision: "skipped",
			wantReason: `unrecognized decision "skipped"`,
		},
		{
			name:       "missing result parks without a cli review",
			wantReason: "abstaining reviewer audit",
		},
		{
			name: "earlier cli review does not resolve", hasResult: true, decision: "skipped",
			cliRecorded: "2026-07-31 12:00:00", wantReason: `unrecognized decision "skipped"`,
		},
		{
			name: "crashed row keeps blocking", state: JobFailed,
			cliRecorded: "2026-07-31 12:02:00", wantReason: "crashed reviewer audit",
		},
		{
			name: "cancelled row keeps blocking", state: JobCancelled,
			cliRecorded: "2026-07-31 12:02:00", wantReason: "crashed reviewer audit",
		},
		{
			name: "queued row keeps waiting", state: JobQueued,
			cliRecorded: "2026-07-31 12:02:00", wantPending: true, wantReason: "waiting for reviewer audit",
		},
		{
			name: "running row keeps waiting", state: JobRunning,
			cliRecorded: "2026-07-31 12:02:00", wantPending: true, wantReason: "waiting for reviewer audit",
		},
		{
			name: "blocking verdict is not a non-verdict", hasResult: true, decision: "changes_requested",
			cliRecorded: "2026-07-31 12:02:00", wantReason: "blocking result from audit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			state := tc.state
			if state == "" {
				state = JobSucceeded
			}
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: engineJobID, agent: "audit", state: state,
				hasResult: tc.hasResult, decision: tc.decision,
				recorded: "2026-07-31 12:01:00",
			})
			if tc.cliRecorded != "" {
				insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
					id: cliJobID, agent: "audit", hasResult: true, decision: "approved",
					recorded: tc.cliRecorded, emptyRound: true,
				})
			}

			decision, err := gate.Evaluate(context.Background(), request)
			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			switch {
			case tc.wantMerged:
				if !decision.Merged {
					t.Fatalf("decision = %+v, want cli verdict to resolve the settled non-verdict round", decision)
				}
				if len(gh.merges) != 1 {
					t.Fatalf("merge calls = %+v, want one", gh.merges)
				}
			case tc.wantPending:
				if decision.Merged || decision.Reason.IsGateMiss() {
					t.Fatalf("decision = %+v, want live reviewer slot to wait without escalating", decision)
				}
			default:
				if decision.Merged || !decision.LeaveOpen || !decision.Reason.IsGateMiss() {
					t.Fatalf("decision = %+v, want the engine round to keep holding the merge", decision)
				}
			}
			if !strings.Contains(decision.Reason.Render(), tc.wantReason) {
				t.Fatalf("decision reason = %q, want %q", decision.Reason, tc.wantReason)
			}
			if !tc.wantMerged && len(gh.merges) != 0 {
				t.Fatalf("merge calls = %+v, want none", gh.merges)
			}
		})
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want failed delegated review evidence parked", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "delegated review parent "+parentID) {
		t.Fatalf("decision reason = %q, want parent job id", decision.Reason)
	}
	for _, delegationID := range []string{
		"lens-anti-drift-correctness",
		"lens-compile-exhaustiveness",
		"lens-tests-regressions",
	} {
		childID := parentID + "/delegation/" + delegationID
		if !strings.Contains(decision.Reason.Render(), childID) {
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
		ID:    parentID,
		Agent: "reviewer-a",
		Type:  "review",
		// Production shape: a delegated review row carries both ids.
		ParentJobID:  "implement-job",
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want fallback approval with failed children parked", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "delegated review parent "+parentID) {
		t.Fatalf("decision reason = %q, want fallback parent job id", decision.Reason)
	}
	for _, delegationID := range []string{"correctness", "compile", "tests"} {
		childID := parentID + "/delegation/" + delegationID
		if !strings.Contains(decision.Reason.Render(), childID) {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want all-skipped delegation evidence parked", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "delegated review parent "+parentID) ||
		!strings.Contains(decision.Reason.Render(), "(skipped)") {
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
			if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
				t.Fatalf("decision = %+v, want unrecognized delegation evidence refused", decision)
			}
			childID := parentID + "/delegation/unrecognized"
			if !strings.Contains(decision.Reason.Render(), "unrecognized delegation evidence") ||
				!strings.Contains(decision.Reason.Render(), childID) {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want mixed unrecognized delegation evidence refused", decision)
	}
	unrecognizedChildID := parentID + "/delegation/unrecognized"
	if !strings.Contains(decision.Reason.Render(), "unrecognized delegation evidence") ||
		!strings.Contains(decision.Reason.Render(), unrecognizedChildID) {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want non-delegation child excluded from evidence", decision)
	}
	skippedChildID := parentID + "/delegation/skipped-delegation"
	if !strings.Contains(decision.Reason.Render(), "no surviving delegation evidence") ||
		!strings.Contains(decision.Reason.Render(), skippedChildID) {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want malformed delegation evidence refused", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "unrecognized delegation evidence") ||
		!strings.Contains(decision.Reason.Render(), childID+" (malformed result)") {
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
			if decision.Merged || decision.LeaveOpen || decision.Reason.IsGateMiss() {
				t.Fatalf("decision = %+v, want active delegation child to wait without escalating", decision)
			}
			childID := parentID + "/delegation/" + tc.name
			if !strings.Contains(decision.Reason.Render(), "waiting for delegated review parent "+parentID) ||
				!strings.Contains(decision.Reason.Render(), childID+" ("+string(tc.state)+")") {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want blocking delegation verdict to stop merge", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "blocking delegation evidence") ||
		!strings.Contains(decision.Reason.Render(), childID+" (changes_requested)") {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want implemented delegation result parked as abstention", decision)
	}
	if !strings.Contains(decision.Reason.Render(), "abstaining delegation children") ||
		!strings.Contains(decision.Reason.Render(), childID+" (implemented)") {
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
			if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
				t.Fatalf("decision = %+v, want adverse sibling to outrank accepted child", decision)
			}
			if !strings.Contains(decision.Reason.Render(), tc.wantReasonClass) ||
				!strings.Contains(decision.Reason.Render(), adverseID+" "+tc.wantDetail) {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || decision.Merged {
		t.Fatalf("decision = %+v, want blocking outcome to win mixed delegation set", decision)
	}
	for _, want := range []string{
		"blocking delegation evidence",
		blockingID + " (blocked)",
		"unrecognized children:",
		unrecognizedID + ` (unrecognized decision "future-review-outcome")`,
	} {
		if !strings.Contains(decision.Reason.Render(), want) {
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
					t.Parallel()

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
						if decision.Merged || decision.LeaveOpen || decision.Reason.IsGateMiss() ||
							!strings.Contains(decision.Reason.Render(), "waiting for delegated review parent") {
							t.Fatalf("decision = %+v, want WAIT", decision)
						}
					case blocks:
						if decision.Merged || !decision.LeaveOpen || !decision.Reason.IsGateMiss() ||
							!strings.Contains(decision.Reason.Render(), "blocking delegation evidence") {
							t.Fatalf("decision = %+v, want BLOCK", decision)
						}
					case parksUnknown:
						if decision.Merged || !decision.LeaveOpen || !decision.Reason.IsGateMiss() ||
							!strings.Contains(decision.Reason.Render(), "unrecognized delegation evidence") {
							t.Fatalf("decision = %+v, want PARK-U", decision)
						}
					case parksCrashed:
						if decision.Merged || !decision.LeaveOpen || !decision.Reason.IsGateMiss() ||
							!strings.Contains(decision.Reason.Render(), "crashed delegation children") {
							t.Fatalf("decision = %+v, want PARK-C", decision)
						}
					case parksAbstaining:
						if decision.Merged || !decision.LeaveOpen || !decision.Reason.IsGateMiss() ||
							!strings.Contains(decision.Reason.Render(), "abstaining delegation children") {
							t.Fatalf("decision = %+v, want PARK-A", decision)
						}
					case parksCouldNotRun:
						if decision.Merged || !decision.LeaveOpen || !decision.Reason.IsGateMiss() ||
							!strings.Contains(decision.Reason.Render(), "parked children:") {
							t.Fatalf("decision = %+v, want PARK STATE_BLOCKED_COULD_NOT_RUN", decision)
						}
					default:
						t.Fatalf("unknown expected outcome %q", want)
					}
					for _, childID := range includedAdverseIDs {
						if !strings.Contains(decision.Reason.Render(), childID) {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || !strings.Contains(decision.Reason.Render(), "different head SHA") {
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
	if !decision.LeaveOpen || !decision.Reason.IsGateMiss() || !strings.Contains(decision.Reason.Render(), "does not record a head SHA") {
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
	insertIndependentMergeGateReview(t, store, db.Job{
		ID:           "review-job",
		Agent:        "audit",
		Type:         "review",
		ParentJobID:  "review-job-implement-author",
		DelegationID: "verify-gate",
	}, JobPayload{
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
	insertIndependentMergeGateReview(t, store, db.Job{
		ID:           "review-job",
		Agent:        "audit",
		Type:         "review",
		ParentJobID:  "review-job-implement-author",
		DelegationID: "verify-gate",
	}, JobPayload{
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
	if decision.Ready || !strings.Contains(decision.Reason.Render(), "different head SHA") {
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
	if decision.Ready || !strings.Contains(decision.Reason.Render(), "review") {
		t.Fatalf("decision = %+v", decision)
	}
}

type mergeConfirmationFailureRunner struct {
	onMerge func()
	calls   int
}

func (r *mergeConfirmationFailureRunner) Run(_ context.Context, _ string, _ string, _ ...string) (subprocess.Result, error) {
	r.calls++
	switch r.calls {
	case 1:
		if r.onMerge != nil {
			r.onMerge()
		}
		return subprocess.Result{Stdout: "merged"}, nil
	case 2:
		return subprocess.Result{Stderr: "HTTP 502"}, errors.New("exit status 1")
	default:
		return subprocess.Result{}, fmt.Errorf("unexpected merge adapter call %d", r.calls)
	}
}

func (*mergeConfirmationFailureRunner) LookPath(file string) (string, error) {
	return file, nil
}

type queuedMergeRunner struct {
	calls int
}

func (r *queuedMergeRunner) Run(_ context.Context, _ string, _ string, _ ...string) (subprocess.Result, error) {
	r.calls++
	switch r.calls {
	case 1:
		return subprocess.Result{Stdout: "queued"}, nil
	case 2:
		return subprocess.Result{Stdout: `{"number":9,"title":"Task","state":"open","html_url":"https://github.com/gitmoot/gitmoot/pull/9","head":{"ref":"task-9","sha":"head123"},"base":{"ref":"main"}}`}, nil
	default:
		return subprocess.Result{}, fmt.Errorf("unexpected queued merge adapter call %d", r.calls)
	}
}

func (*queuedMergeRunner) LookPath(file string) (string, error) {
	return file, nil
}

type productionMergeGateGitHub struct {
	*fakeMergeGateGitHub
	mergeClient github.GhClient
}

func (g *productionMergeGateGitHub) MergePullRequest(ctx context.Context, input github.MergePullRequestInput) (github.MergeResult, error) {
	g.merges = append(g.merges, input)
	g.operations = append(g.operations, "merge")
	return g.mergeClient.MergePullRequest(ctx, input)
}

type fakeMergeGateGitHub struct {
	pr             github.PullRequest
	status         github.CombinedStatus
	compare        github.CompareResult
	checks         []github.PullRequestCheck
	files          []github.PullRequestFile
	mergeResult    github.MergeResult
	getPullRequest func(int) (github.PullRequest, error)
	mergeErr       error
	statusErr      error
	beforeMerge    func()
	updateErr      error
	statuses       []github.CommitStatusInput
	merges         []github.MergePullRequestInput
	updates        []github.UpdatePullRequestBranchInput
	comments       []string
	operations     []string
	getCalls       int
	statusCalls    int
	compareCalls   int
	checkCalls     int
	prCheckCalls   int
	checkRefs      []string
	noChecks       bool
	strictBase     bool
	strictKnown    bool
	strictErr      error
	strictCalls    int
	strictBranches []string
}

func (f *fakeMergeGateGitHub) GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error) {
	f.getCalls++
	if f.getPullRequest != nil {
		return f.getPullRequest(f.getCalls)
	}
	return f.pr, nil
}

func (f *fakeMergeGateGitHub) ListPullRequestFiles(context.Context, github.Repository, int64) ([]github.PullRequestFile, error) {
	return append([]github.PullRequestFile(nil), f.files...), nil
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

func (f *fakeMergeGateGitHub) BaseRequiresUpToDateHead(_ context.Context, _ github.Repository, branch string) (bool, bool, error) {
	f.strictCalls++
	f.strictBranches = append(f.strictBranches, branch)
	if f.strictErr != nil {
		return false, false, f.strictErr
	}
	return f.strictBase, f.strictKnown, nil
}

func (f *fakeMergeGateGitHub) UpdatePullRequestBranch(_ context.Context, input github.UpdatePullRequestBranchInput) (github.UpdatePullRequestBranchResult, error) {
	f.updates = append(f.updates, input)
	return github.UpdatePullRequestBranchResult{Message: "Updating pull request branch."}, f.updateErr
}

func (f *fakeMergeGateGitHub) MergePullRequest(_ context.Context, input github.MergePullRequestInput) (github.MergeResult, error) {
	if f.beforeMerge != nil {
		f.beforeMerge()
	}
	f.merges = append(f.merges, input)
	f.operations = append(f.operations, "merge")
	return f.mergeResult, f.mergeErr
}

type fakeMergeGateGit struct {
	clean    bool
	onClean  func()
	onUpdate func()
	updated  []string
}

func (f *fakeMergeGateGit) WorktreeClean(context.Context) (bool, error) {
	if f.onClean != nil {
		f.onClean()
	}
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

// setMergeGateJobTimestamps stamps created_at and updated_at to the SAME value,
// which is what a row that ran once looks like.
func setMergeGateJobTimestamps(t *testing.T, store *db.Store, jobID string, timestamp string) {
	t.Helper()
	setMergeGateJobRecordedTimes(t, store, jobID, timestamp, timestamp)
}

// setMergeGateJobRecordedTimes stamps created_at and updated_at INDEPENDENTLY; an
// empty value leaves that column untouched. This is the only way a fixture can
// express a row retried in place, where created_at is old and updated_at is new,
// and it is exactly the shape no merge-gate fixture could express before.
func setMergeGateJobRecordedTimes(t *testing.T, store *db.Store, jobID string, createdAt string, updatedAt string) {
	t.Helper()
	if createdAt == "" && updatedAt == "" {
		t.Fatalf("set job %s recorded times: both values empty", jobID)
	}
	raw, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("open store for timestamp update: %v", err)
	}
	defer raw.Close()
	assignments := make([]string, 0, 2)
	args := make([]any, 0, 3)
	if createdAt != "" {
		assignments = append(assignments, "created_at = ?")
		args = append(args, createdAt)
	}
	if updatedAt != "" {
		assignments = append(assignments, "updated_at = ?")
		args = append(args, updatedAt)
	}
	args = append(args, jobID)
	result, err := raw.ExecContext(context.Background(),
		`UPDATE jobs SET `+strings.Join(assignments, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		t.Fatalf("set job %s recorded times: %v", jobID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		t.Fatalf("set job %s recorded times affected=%d, err=%v; want 1", jobID, affected, err)
	}
}

// settleMergeGateReviewInPlace mirrors `gitmoot job retry` settling a review row:
// the SAME row transitions to a state carrying (or losing) a result, keeping its
// id, head, round and CREATED_AT while only updated_at advances. No production
// path adds a second row for the same round.
func settleMergeGateReviewInPlace(t *testing.T, store *db.Store, jobID string, state JobState, result *AgentResult, recorded string) {
	t.Helper()
	ctx := context.Background()
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s) returned error: %v", jobID, err)
	}
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(%s) returned error: %v", jobID, err)
	}
	payload.Result = result
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload(%s) returned error: %v", jobID, err)
	}
	if err := store.UpdateJobPayload(ctx, jobID, encoded); err != nil {
		t.Fatalf("UpdateJobPayload(%s) returned error: %v", jobID, err)
	}
	if err := store.UpdateJobState(ctx, jobID, string(state)); err != nil {
		t.Fatalf("UpdateJobState(%s) returned error: %v", jobID, err)
	}
	setMergeGateJobRecordedTimes(t, store, jobID, "", recorded)
}

// #1685 P2. The gate groups delegation children by parent and judges every row.
// A panel child that FAILED and was then retried to approval left the obsolete
// failed row in that set forever: a terminal row can never change, so the panel
// stayed poisoned and only a new head could clear it. Continuation synthesis
// already selects the highest RetryCount per delegation; the gate must agree.
func TestPolicyMergeGateUsesLatestDelegationAttempt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		latestState JobState
		latest      *AgentResult
		wantErr     bool
	}{
		{
			name: "approved retry supersedes the failed original", latestState: JobSucceeded,
			latest: &AgentResult{Decision: "approved", Summary: "retry verified the head"},
		},
		{
			name: "a failed latest attempt still blocks", latestState: JobFailed, wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			insertMergeGateFanOutRow(t, store, "approved")
			// lens-b answers cleanly throughout; only lens-a is retried, so a mixed
			// panel proves the selection is per-delegation and not per-parent.
			insertMergeGatePanelChild(t, store, "review-panel", "lens-b", JobSucceeded, &AgentResult{
				Decision: "approved", Summary: "lens-b verified the head",
			})
			insertMergeGatePanelChild(t, store, "review-panel", "lens-c", JobSucceeded, &AgentResult{
				Decision: "approved", Summary: "lens-c verified the head",
			})
			insertMergeGatePanelChild(t, store, "review-panel", "lens-a", JobFailed, nil)
			insertMergeGatePanelRetry(t, store, "review-panel", "lens-a", 1, tc.latestState, tc.latest)

			err := (PolicyMergeGate{Store: store}).ensureFinalReviewCaptured(ctx, MergeRequest{
				Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", Reviewer: "g6-review-sol",
			}, "head123")
			if tc.wantErr {
				if err == nil {
					t.Fatal("an unsuccessful latest attempt must still block the panel")
				}
				return
			}
			if err != nil {
				t.Fatalf("an approved retry must clear its failed original, got %v", err)
			}
		})
	}
}

// insertMergeGatePanelRetry adds a later ATTEMPT of an existing delegation, the
// shape the engine writes on retry: same DelegationID, RetryCount incremented,
// a distinct job id.
func insertMergeGatePanelRetry(t *testing.T, store *db.Store, parentID, delegationID string, attempt int, state JobState, result *AgentResult) {
	t.Helper()
	encoded, err := marshalPayload(JobPayload{
		Repo: "mobile/app", PullRequest: 9, TaskID: "task-9", RetryCount: attempt, Result: result,
	})
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.CreateJobWithEvent(context.Background(), db.Job{
		ID:           fmt.Sprintf("%s/delegation/%s/retry/%d", parentID, delegationID, attempt),
		Agent:        delegationID,
		Type:         "review",
		State:        string(state),
		Payload:      encoded,
		ParentJobID:  parentID,
		DelegationID: delegationID,
	}, db.JobEvent{Kind: string(state), Message: "delegation retry fixture"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
}

// insertMergeGateHeadlessReview seeds a review row with NO HeadSHA, which
// insertMergeGateReviewFixture cannot express: it defaults an empty head to
// "head123". A headless row is what `gitmoot agent review` leaves behind when the
// head is not recorded, and it is the row issue #1933 turns on.
func insertMergeGateHeadlessReview(t *testing.T, store *db.Store, id, agent, decision, taskID, round string) {
	t.Helper()
	insertCompletedJob(t, store, db.Job{ID: id, Agent: agent, Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "",
		TaskID:      taskID,
		ReviewRound: round,
		Result:      &AgentResult{Decision: decision, Summary: "headless verdict"},
	})
}

// TestPolicyMergeGateBlocksHeadlessObjectionAheadOfTheExternalMergeClaim is
// issue #1933, and it enters through Evaluate rather than
// ensureFinalReviewCaptured because the defect IS the ordering between the
// review evaluation and the external-merge state claim - a test that called the
// helper could not observe the claim at all.
//
// The interleaving, from the issue: a succeeded current-head APPROVAL and a
// succeeded ordinary HEADLESS changes_requested row on the same task.
// reviewsAtHead holds only the approval, because the strict evaluated-head
// filter drops the headless row; the latest-round fallback never runs because
// the strict population is non-empty; ClaimTaskState then fences the objection's
// own transition and the merge completes with the objection unconsumed.
//
// Both orderings are asserted because the store returns rows by id and the
// defect must not be reachable by seeding them the other way round.
func TestPolicyMergeGateBlocksHeadlessObjectionAheadOfTheExternalMergeClaim(t *testing.T) {
	for _, tt := range []struct {
		name           string
		objectionFirst bool
	}{
		{name: "approval recorded before the headless objection", objectionFirst: false},
		{name: "headless objection recorded before the approval", objectionFirst: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			if tt.objectionFirst {
				insertMergeGateHeadlessReview(t, store, "a-review-objection", "gm-review-opus", "changes_requested", "task-9", "review-1")
				insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
					id: "b-review-approval", agent: "audit", decision: "approved", hasResult: true,
				})
			} else {
				insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
					id: "a-review-approval", agent: "audit", decision: "approved", hasResult: true,
				})
				insertMergeGateHeadlessReview(t, store, "b-review-objection", "gm-review-opus", "changes_requested", "task-9", "review-1")
			}
			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
				State: string(TaskReadyToMerge),
			}); err != nil {
				t.Fatal(err)
			}
			request.Reviewer = "audit"
			request.ExpectedTaskState = string(TaskReadyToMerge)

			decision, err := gate.Evaluate(ctx, request)

			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if decision.Merged || len(gh.merges) != 0 {
				t.Fatalf("decision=%+v merges=%d, want NO merge: a persisted headless changes_requested objection must block BEFORE the external-merge claim (#1933)",
					decision, len(gh.merges))
			}
			// The claim must never have been taken, or the objection's own advance
			// would still be fenced even though the merge was refused.
			task, taskErr := store.GetTask(ctx, "task-9")
			if taskErr != nil {
				t.Fatalf("GetTask: %v", taskErr)
			}
			if task.State != string(TaskReadyToMerge) {
				t.Fatalf("task state = %q, want %q: the gate must not claim the task state when a headless objection blocks",
					task.State, string(TaskReadyToMerge))
			}
		})
	}
}

// TestPolicyMergeGateStillMergesWhenAHeadlessObjectionDoesNotApply is the
// should-SUCCEED half of #1933, and it is the half that decides whether the fix
// is a gate or an outage. Every qualifier in the new block gets an arm here, and
// each arm must still reach a completed external merge.
//
// One honest mapping, because the vocabulary does not contain the words: a
// review row has no "answered" or "withdrawn" DECISION - isReviewReplacementDecision
// covers approved/changes_requested/blocked/failed only. At row level both are
// expressed the same way, by a strictly later terminal verdict from the SAME
// reviewer superseding the objection, which is what the first two arms drive.
// The findings ledger's own db.FindingAnswered/db.FindingWithdrawn states are a
// different surface and are enforced by EnsureLedgerObligationsObserved earlier
// in this same function, not by this block.
func TestPolicyMergeGateStillMergesWhenAHeadlessObjectionDoesNotApply(t *testing.T) {
	for _, tt := range []struct {
		name string
		seed func(t *testing.T, store *db.Store)
		why  string
		// requestBranch populates MergeRequest.Branch, without which
		// sameCorrelatedTask's branch comparison is unreachable: an empty side is
		// treated as no evidence rather than a mismatch.
		requestBranch string
		// wantMerge defaults to TRUE - this table is the should-SUCCEED half - and
		// is set false only by the arms guarding corrections to it.
		wantMerge *bool
		// wantReasonContains pins WHICH path refused, not merely that one did: a
		// block from the headless pre-pass and a block from the parent's delegation
		// evidence are different guarantees, and shadowing one with the other would
		// pass a bare "it did not merge" assertion.
		wantReasonContains string
	}{
		{
			name: "answered: the same reviewer later approved, superseding it",
			why:  "a strictly later terminal verdict from the same reviewer is how an objection is answered",
			seed: func(t *testing.T, store *db.Store) {
				insertMergeGateHeadlessReviewAt(t, store, "objection-early", "gm-review-opus", "changes_requested", "task-9", "review-1", "2026-09-01T10:00:00Z")
				insertMergeGateHeadlessReviewAt(t, store, "objection-answered", "gm-review-opus", "approved", "task-9", "review-1", "2026-09-01T12:00:00Z")
			},
		},
		{
			name: "withdrawn: the same reviewer replaced it at the evaluated head",
			why:  "withdrawal is expressed as a later terminal verdict from the same reviewer, here rendered at the head",
			seed: func(t *testing.T, store *db.Store) {
				insertMergeGateHeadlessReviewAt(t, store, "objection-withdrawn", "gm-review-opus", "changes_requested", "task-9", "review-1", "2026-09-01T10:00:00Z")
				// A strictly LATER ROUND, not an empty one: reviewRoundKey refuses to
				// order one explicit review-N against an empty round, so an empty-round
				// replacement supersedes nothing and the objection correctly still
				// blocks. The first version of this arm made that mistake and failed.
				insertCompletedJob(t, store, db.Job{ID: "objection-replacement", Agent: "gm-review-opus", Type: "review"}, JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "head123", TaskID: "task-9",
					ReviewRound: "review-2",
					Result:      &AgentResult{Decision: "approved", Summary: "withdrawn and replaced"},
				})
			},
		},
		{
			name: "superseded: a later headless verdict from the same reviewer wins",
			why:  "supersession is same-reviewer and strictly-later, and it must clear the earlier row",
			seed: func(t *testing.T, store *db.Store) {
				insertMergeGateHeadlessReviewAt(t, store, "objection-superseded", "gm-review-opus", "changes_requested", "task-9", "review-1", "2026-09-01T09:00:00Z")
				insertMergeGateHeadlessReviewAt(t, store, "objection-latest", "gm-review-opus", "approved", "task-9", "review-1", "2026-09-01T11:00:00Z")
			},
		},
		{
			name: "non-applicable: the objection belongs to a different pull request",
			why:  "sameCorrelatedTask refuses a row from another pull request outright",
			seed: func(t *testing.T, store *db.Store) {
				insertCompletedJob(t, store, db.Job{ID: "objection-other-pr", Agent: "gm-review-opus", Type: "review"}, JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 8, HeadSHA: "", TaskID: "task-8",
					Result: &AgentResult{Decision: "changes_requested", Summary: "another PR's objection"},
				})
			},
		},
		{
			name: "non-applicable: the objection's branch disagrees with the request's",
			why:  "when both sides record a branch and they differ, the row belongs to another lane (#1519)",
			// A DIFFERENT TASK ID ALONE IS NOT NON-APPLICABLE. sameCorrelatedTask
			// correlates on repo+PR whenever the branches do not disagree, REGARDLESS
			// of task id, because review identity migrates between rounds (#1519). An
			// earlier version of this arm seeded only a divergent task id and expected
			// a merge; it FAILED, correctly, and weakening the guard to satisfy it
			// would have reopened the very bypass this block exists to close. The arm
			// below pins that reading so the mistake cannot be made again silently.
			requestBranch: "task-9",
			seed: func(t *testing.T, store *db.Store) {
				insertCompletedJob(t, store, db.Job{ID: "objection-other-branch", Agent: "gm-review-opus", Type: "review"}, JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 9, Branch: "some-other-branch", HeadSHA: "", TaskID: "review-pr-9-3f3a1026",
					Result: &AgentResult{Decision: "changes_requested", Summary: "another branch's objection"},
				})
			},
		},
		{
			name:      "APPLICABLE despite a migrated task id: this one must BLOCK",
			why:       "a divergent task id on the same PR still correlates (#1519), so the objection still applies",
			wantMerge: boolPtr(false),
			seed: func(t *testing.T, store *db.Store) {
				insertMergeGateHeadlessReview(t, store, "objection-migrated-id", "gm-review-opus", "changes_requested", "review-pr-9-3f3a1026", "review-1")
			},
		},
		{
			name: "rendered against another head: not headless at all",
			why:  "a head-bound objection is owned by the strict evaluated-head population, not by this block",
			seed: func(t *testing.T, store *db.Store) {
				insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
					id: "objection-other-head", agent: "gm-review-opus", decision: "changes_requested",
					hasResult: true, headSHA: "someotherhead",
				})
			},
		},
		{
			name: "not ordinary: a headless fan-out announcement is not a verdict",
			why:  "#1685 - a row declaring delegations announces a panel and never answers for the head",
			seed: func(t *testing.T, store *db.Store) {
				insertCompletedJob(t, store, db.Job{ID: "objection-fanout", Agent: "gm-review-opus", Type: "review"}, JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "", TaskID: "task-9",
					Result: &AgentResult{
						Decision:    "changes_requested",
						Summary:     "fan-out announcement",
						Delegations: []Delegation{{ID: "lens-a", Agent: "lens-a", Action: "review", Prompt: "look at it"}},
					},
				})
			},
		},
		{
			name: "not applicable: an integration-worktree objection records no head by design",
			why:  "#332/#388 - the engine CLEARS the head for these children, so treating one as an objection against this head makes it an objection against every head, which no push can clear",
			// The pre-existing TestPolicyMergeGateHeadlessIntegrationObjectionDoesNotMatchEveryHead
			// already holds this line and I did not touch it. This arm exists so the
			// exclusion is bound from inside the #1933 suite as well: deleting it must
			// fail a test written by the change that depends on it, not only an
			// inherited one.
			seed: func(t *testing.T, store *db.Store) {
				insertCompletedJob(t, store, db.Job{ID: "objection-integration", Agent: "objector", Type: "review"}, JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", ReviewRound: "review-1",
					DelegationID: "verify-old",
					WorktreePath: "/tmp/gitmoot/integration-verify-old",
					Result:       &AgentResult{Decision: "changes_requested", Summary: "integration objection"},
				})
			},
		},
		{
			name:               "a delegation child blocks through its PARENT'S evidence, not through this block",
			why:                "the engine clears only the inherited HeadSHA so a real child keeps its round; it must be judged by ensureDelegatedReviewEvidence, and the headless block must not shadow that path with its own reason",
			wantMerge:          boolPtr(false),
			wantReasonContains: "blocking delegation evidence",
			seed: func(t *testing.T, store *db.Store) {
				encoded, err := marshalPayload(JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", ReviewRound: "review-1",
					Result: &AgentResult{Decision: "changes_requested", Summary: "delegated child verdict"},
				})
				if err != nil {
					t.Fatalf("marshalPayload: %v", err)
				}
				if err := store.CreateJobWithEvent(context.Background(), db.Job{
					ID: "objection-delegation-child", Agent: "lens-a", Type: "review",
					State: string(JobSucceeded), Payload: encoded,
					ParentJobID: "review-approval", DelegationID: "lens-a",
				}, db.JobEvent{Kind: string(JobSucceeded), Message: "child"}); err != nil {
					t.Fatalf("CreateJobWithEvent: %v", err)
				}
			},
		},
		{
			// PINS THE DELEGATION-CHILD EXCLUSION ITSELF. The arm above it does not:
			// its parent is a review row, so isRoundHistoryDuplicate already excludes
			// it and deleting isDelegationChild changed nothing - a surviving mutant
			// proved that. A child of the IMPLEMENT row is not round history, so this
			// is the shape where the exclusion is the only thing standing between a
			// headless child and a permanent block.
			name: "not applicable: a delegation child whose parent is not a review row",
			why:  "the engine clears a child's inherited head; a child of the implement row is not round history, so only the delegation-child exclusion keeps it from blocking every head",
			seed: func(t *testing.T, store *db.Store) {
				encoded, err := marshalPayload(JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", ReviewRound: "review-1",
					Result: &AgentResult{Decision: "changes_requested", Summary: "verify child of the implement row"},
				})
				if err != nil {
					t.Fatalf("marshalPayload: %v", err)
				}
				if err := store.CreateJobWithEvent(context.Background(), db.Job{
					ID: "objection-child-of-implement", Agent: "verifier", Type: "review",
					State: string(JobSucceeded), Payload: encoded,
					ParentJobID: "implement-job", DelegationID: "verify",
				}, db.JobEvent{Kind: string(JobSucceeded), Message: "child"}); err != nil {
					t.Fatalf("CreateJobWithEvent: %v", err)
				}
			},
		},
		{
			// PINS "LATEST". With two qualifying objections the reason must name the
			// NEWER one; reversing the selection was a surviving mutant until this arm
			// existed, because every other scenario has at most one candidate.
			name:               "two qualifying objections: the LATEST one is the one reported",
			why:                "latest is decided by the same reviewRoundKey ordering supersession uses, so the block must name the newer reviewer",
			wantMerge:          boolPtr(false),
			wantReasonContains: "later-objector",
			seed: func(t *testing.T, store *db.Store) {
				insertMergeGateHeadlessReviewAt(t, store, "objection-older", "earlier-objector", "changes_requested", "task-9", "review-1", "2026-09-01T09:00:00Z")
				insertMergeGateHeadlessReviewAt(t, store, "objection-newer", "later-objector", "changes_requested", "task-9", "review-2", "2026-09-01T15:00:00Z")
			},
		},
		{
			name: "not applicable: neither head nor round is an unattributable remnant",
			why:  "matches no production writer of a reviewer verdict - engine reviews carry a round, CLI reviews carry a head - and TestPolicyMergeGateDelegatedReviewEvidenceEnumeration asserts wantExcluded for exactly this shape",
			seed: func(t *testing.T, store *db.Store) {
				insertCompletedJob(t, store, db.Job{ID: "objection-no-round", Agent: "orphan", Type: "review"}, JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
					Result: &AgentResult{Decision: "changes_requested", Summary: "unattributable remnant"},
				})
			},
		},
		{
			// PINS THE changes_requested NARROWNESS. Widening this block to "anything
			// not approved" was a surviving mutant until this arm existed, and the
			// widening is not harmless: an ABSTENTION cannot be answered or superseded
			// by its author, so blocking on one would deadlock with no operator move.
			name: "not a changes_requested verdict: a headless abstention must not block",
			why:  "an abstention is not an objection, and blocking on one would be unanswerable",
			seed: func(t *testing.T, store *db.Store) {
				insertMergeGateHeadlessReviewAt(t, store, "objection-abstained", "abstainer", "skipped", "task-9", "review-1", "2026-09-01T10:00:00Z")
			},
		},
		{
			name: "not succeeded: a queued headless row is not a verdict",
			why:  "an unfinished row has rendered no objection; rows at the evaluated head are judged above",
			seed: func(t *testing.T, store *db.Store) {
				encoded, err := marshalPayload(JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "", TaskID: "task-9",
					// A ROUND IS REQUIRED for this arm to reach the succeeded-state
					// check: without it the round qualifier excludes the row first and
					// the arm cannot fail, which a surviving mutant proved.
					ReviewRound: "review-1",
					Result:      &AgentResult{Decision: "changes_requested", Summary: "still running"},
				})
				if err != nil {
					t.Fatalf("marshalPayload: %v", err)
				}
				if err := store.CreateJobWithEvent(context.Background(), db.Job{
					ID: "objection-queued", Agent: "gm-review-opus", Type: "review",
					State: string(JobQueued), Payload: encoded,
				}, db.JobEvent{Kind: string(JobQueued), Message: "queued"}); err != nil {
					t.Fatalf("CreateJobWithEvent: %v", err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: "review-approval", agent: "audit", decision: "approved", hasResult: true,
			})
			tt.seed(t, store)
			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
				State: string(TaskReadyToMerge),
			}); err != nil {
				t.Fatal(err)
			}
			request.Reviewer = "audit"
			request.ExpectedTaskState = string(TaskReadyToMerge)
			request.Branch = tt.requestBranch

			decision, err := gate.Evaluate(ctx, request)

			if err != nil {
				t.Fatalf("Evaluate returned error: %v (%s)", err, tt.why)
			}
			wantMerge := tt.wantMerge == nil || *tt.wantMerge
			if wantMerge && (!decision.Merged || len(gh.merges) != 1) {
				t.Fatalf("decision=%+v merges=%d, want ONE completed merge: %s", decision, len(gh.merges), tt.why)
			}
			if !wantMerge && (decision.Merged || len(gh.merges) != 0) {
				t.Fatalf("decision=%+v merges=%d, want NO merge: %s", decision, len(gh.merges), tt.why)
			}
			if tt.wantReasonContains != "" && !strings.Contains(decision.Reason.Render(), tt.wantReasonContains) {
				t.Fatalf("reason = %q, want it to contain %q: %s", decision.Reason.Render(), tt.wantReasonContains, tt.why)
			}
		})
	}
}

// insertMergeGateHeadlessReviewAt is insertMergeGateHeadlessReview with the
// recorded timestamps pinned, which is the only way to make supersession
// decidable between two headless rows: reviewRoundKeyForJob falls back to
// UpdatedAt then CreatedAt when no round is set, and rows whose order cannot be
// established supersede in neither direction by design.
func insertMergeGateHeadlessReviewAt(t *testing.T, store *db.Store, id, agent, decision, taskID, round, recorded string) {
	t.Helper()
	insertMergeGateHeadlessReview(t, store, id, agent, decision, taskID, round)
	setMergeGateJobTimestamps(t, store, id, recorded)
}

// boolPtr expresses an explicit false in a table whose default is true.
func boolPtr(v bool) *bool { return &v }

// TestPolicyMergeGateBlocksASessionReviewObjectionWithNeitherHeadNorRound is
// #1950's P1 F1, and it is built through the PRODUCTION session writers rather
// than a hand-assembled row, because the whole point is what those writers
// actually persist: OpenExternalJob stores neither HeadSHA nor ReviewRound and
// demotes any supplied head to a display event (session_job.go:85-125), and
// CloseExternalJobWithUsage then stores the verdict and succeeds the row
// (session_job.go:170-190). That is `gitmoot job record --type review`
// (internal/cli/job_session.go:214-305).
//
// A predicate that excluded roundless rows dropped this real objection and let
// the external merge complete. The discriminator is ORIGIN - the row is
// externally driven - never the absence of a round.
func TestPolicyMergeGateBlocksASessionReviewObjectionWithNeitherHeadNorRound(t *testing.T) {
	ctx := context.Background()
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-approval", agent: "audit", decision: "approved", hasResult: true,
	})

	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	if _, err := mailbox.OpenExternalJob(ctx, JobRequest{
		ID:          "session-review-objection",
		Agent:       "session-reviewer",
		Action:      "review",
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		TaskID:      "task-9",
		HeadSHA:     "head123", // demoted to a display event on purpose
		Sender:      "session",
	}); err != nil {
		t.Fatalf("OpenExternalJob returned error: %v", err)
	}
	closed, err := mailbox.CloseExternalJobWithUsage(ctx, "session-review-objection", AgentResult{
		Decision: "changes_requested", Severity: reviewseverity.P1, Summary: "session objection",
	}, 0, "", "", ExternalJobUsage{})
	if err != nil {
		t.Fatalf("CloseExternalJobWithUsage returned error: %v", err)
	}
	// The writers really do leave both fields empty - assert it rather than trust
	// the comment, since the whole finding turned on this being true.
	closedPayload, err := unmarshalPayload(closed.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	if strings.TrimSpace(closedPayload.HeadSHA) != "" || strings.TrimSpace(closedPayload.ReviewRound) != "" {
		t.Fatalf("session review payload head=%q round=%q, want both empty; the fixture no longer reproduces the finding",
			closedPayload.HeadSHA, closedPayload.ReviewRound)
	}
	if !closed.ExternallyDriven {
		t.Fatalf("session review job ExternallyDriven=false, want true; the discriminator this fix relies on is absent")
	}

	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	request.Reviewer = "audit"
	request.ExpectedTaskState = string(TaskReadyToMerge)

	decision, evalErr := gate.Evaluate(ctx, request)

	if evalErr != nil {
		t.Fatalf("Evaluate returned error: %v", evalErr)
	}
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("decision=%+v merges=%d, want NO merge: a session review objection carrying neither head nor round is a real blocking verdict (#1933/#1950 F1)",
			decision, len(gh.merges))
	}
	task, taskErr := store.GetTask(ctx, "task-9")
	if taskErr != nil {
		t.Fatalf("GetTask: %v", taskErr)
	}
	if task.State != string(TaskReadyToMerge) {
		t.Fatalf("task state = %q, want %q: the gate must not transition or claim the task state when a session objection blocks",
			task.State, string(TaskReadyToMerge))
	}
}

// openSessionReviewObjection records a changes_requested session review through
// the PRODUCTION writers. agent and actingRole are passed exactly as a caller
// would: OpenExternalJob supports a role IN PLACE OF an agent, and that shape is
// what #1950 F2 turned on.
func openSessionReviewObjection(t *testing.T, store *db.Store, id, agent, actingRole, decision string, severity string) {
	t.Helper()
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	if _, err := mailbox.OpenExternalJob(context.Background(), JobRequest{
		ID:            id,
		Agent:         agent,
		ActingOrgRole: actingRole,
		Action:        "review",
		Repo:          "gitmoot/gitmoot",
		PullRequest:   9,
		TaskID:        "task-9",
		Sender:        "session",
	}); err != nil {
		t.Fatalf("OpenExternalJob(%s) returned error: %v", id, err)
	}
	result := AgentResult{Decision: decision, Summary: "session verdict"}
	if severity != "" {
		result.Severity = severity
	}
	if _, err := mailbox.CloseExternalJobWithUsage(context.Background(), id, result, 0, "", "", ExternalJobUsage{}); err != nil {
		t.Fatalf("CloseExternalJobWithUsage(%s) returned error: %v", id, err)
	}
}

// TestPolicyMergeGateAllowsAnActingRoleSessionObjectionToBeSuperseded is #1950
// F2: OpenExternalJob supports ActingOrgRole in place of Agent, persisting
// Agent="" with the normalized role. The headless scan derived reviewer identity
// from job.Agent alone, so with an empty agent supersession was SKIPPED - a
// role's objection could never be answered by that role's own later approval,
// which is a block with no operator move available.
//
// Both arms run through the production writers. The blocking arm also asserts the
// refusal NAMES the role: rendering "objection from  " tells an operator nothing.
func TestPolicyMergeGateAllowsAnActingRoleSessionObjectionToBeSuperseded(t *testing.T) {
	for _, tt := range []struct {
		name        string
		addApproval bool
		wantMerge   bool
		// noIdentity drops BOTH the agent and the acting role, which is the shape
		// that can never be superseded and therefore must never be excluded.
		noIdentity bool
	}{
		{name: "the role's objection alone still blocks, and names the role", addApproval: false, wantMerge: false},
		{name: "a later approval from the SAME role supersedes it", addApproval: true, wantMerge: true},
		{
			// ROUND 1 MUST NOT BE WEAKENED. A row with no usable identity at all -
			// neither agent nor acting role - can never be superseded by anyone, so it
			// must still BLOCK rather than be skipped. Under-blocking is how F1
			// happened; this arm is what stops the identity fix from re-introducing it.
			name: "DEFENSIVE INVARIANT (no supported writer produces this): a row with no usable identity still blocks", noIdentity: true, wantMerge: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: "review-approval", agent: "audit", decision: "approved", hasResult: true,
			})
			if tt.noIdentity {
				// THIS ARM IS A DEFENSIVE INVARIANT, NOT PRODUCTION-WRITER COVERAGE, and
				// the previous comment here overclaimed by calling the row "reachable".
				// #1950 F3 is right: no supported writer can produce Type=review with a
				// round, an empty Agent AND an empty ActingOrgRole. OpenExternalJob
				// refuses it outright - pinned by
				// TestOpenExternalJobRefusesAReviewWithNeitherAgentNorActingRole below -
				// and every ordinary engine, CLI, daemon, pipeline, delegation and retry
				// insert goes through Mailbox validation, which requires an agent.
				//
				// It is asserted anyway because the gate must FAIL CLOSED on a row it
				// cannot attribute, whatever produced it: a legacy row from before that
				// validation, a corrupted payload, or a future writer nobody has
				// enumerated. insertCompletedJob deliberately bypasses the validators to
				// construct that state, which is exactly why this is labelled a
				// corruption/legacy invariant rather than evidence about production.
				insertCompletedJob(t, store, db.Job{ID: "session-role-objection", Agent: "", Type: "review"}, JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", ReviewRound: "review-1",
					Result: &AgentResult{Decision: "changes_requested", Severity: reviewseverity.P1, Summary: "authorless objection"},
				})
			} else {
				openSessionReviewObjection(t, store, "session-role-objection", "", "reviewer", "changes_requested", reviewseverity.P1)
			}
			if tt.addApproval {
				// Same ROLE, no agent, strictly later: this is the only move the
				// operator has, and before the fix it could not clear the block.
				setMergeGateJobTimestamps(t, store, "session-role-objection", "2026-09-01T10:00:00Z")
				openSessionReviewObjection(t, store, "session-role-approval", "", "reviewer", "approved", "")
				setMergeGateJobTimestamps(t, store, "session-role-approval", "2026-09-01T14:00:00Z")
			}
			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
				State: string(TaskReadyToMerge),
			}); err != nil {
				t.Fatal(err)
			}
			request.Reviewer = "audit"
			request.ExpectedTaskState = string(TaskReadyToMerge)

			decision, err := gate.Evaluate(ctx, request)
			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if tt.wantMerge && (!decision.Merged || len(gh.merges) != 1) {
				t.Fatalf("decision=%+v merges=%d, want ONE merge: a later approval from the same acting role must supersede that role's objection (#1950 F2)",
					decision, len(gh.merges))
			}
			if !tt.wantMerge {
				if decision.Merged || len(gh.merges) != 0 {
					t.Fatalf("decision=%+v merges=%d, want NO merge: an acting-role session objection is a real blocking verdict", decision, len(gh.merges))
				}
				wantAuthor := "reviewer"
				if tt.noIdentity {
					// Never an empty name: that is the line a human reads while working
					// out why a merge is stuck.
					wantAuthor = "an unattributed reviewer"
				}
				rendered := decision.Reason.Render()
				if !strings.Contains(rendered, wantAuthor) {
					t.Fatalf("reason = %q, want %q named; an empty author leaves an operator with nobody to go to", rendered, wantAuthor)
				}
				if !tt.noIdentity && strings.Contains(rendered, "an unattributed reviewer") {
					t.Fatalf("reason = %q, want the ROLE named and NOT the unattributed fallback; matching %q alone also accepts \"an unattributed reviewer\", which is why a diagnostic-only revert stayed green (#1950 F4)", rendered, wantAuthor)
				}
			}
		})
	}
}

// TestPolicyMergeGateAttributesAnImplementRowToItsAgentNotItsDispatchingRole
// pins the AGENT-FIRST half of effectiveReviewerIdentity, which nothing in the
// package covered: inverting that order so a role wins passed the entire
// internal/workflow suite. Extracting the rule into one shared function (#1950
// F4) concentrates the risk, so the order needs its own guard.
//
// The hazard is the one the collector's own comment names. An ordinary dispatched
// implement row carries the DISPATCHING coordinator's ActingOrgRole in its
// payload. Read the role first and that coordinator becomes an implementer of
// everything - and is then disqualified from reviewing anything. This asserts the
// consequence rather than the mechanism: the coordinator's own approval must
// still count as independent, so the merge proceeds.
func TestPolicyMergeGateAttributesAnImplementRowToItsAgentNotItsDispatchingRole(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "wave-impl", Type: "implement"}, JobPayload{
		Repo:          "gitmoot/gitmoot",
		PullRequest:   9,
		TaskID:        "task-9",
		ActingOrgRole: "coordinator", // the DISPATCHER's role, not the implementer
		Result:        &AgentResult{Decision: "implemented", Summary: "implemented"},
	})
	// AGENT-authored, and its name deliberately COLLIDES with the dispatching role
	// above. That collision is what makes the attribution order observable: with
	// agent-first the implement row belongs to wave-impl and this reviewer is
	// independent, while role-first would make "coordinator" the implementer and
	// turn this into self-approval. An author-LESS approval cannot be used here -
	// the current-head arm refuses it as "no recorded reviewer author", which is a
	// different guard and would test the wrong thing.
	insertCompletedJob(t, store, db.Job{ID: "review-role-approval", Agent: "coordinator", Type: "review"}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		PullRequest: 9,
		HeadSHA:     "head123",
		TaskID:      "task-9",
		ReviewRound: "review-1",
		Result:      &AgentResult{Decision: "approved", Summary: "approved by an agent named for the role"},
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

	decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if !decision.Merged || len(gh.merges) != 1 {
		t.Fatalf("decision=%+v merges=%d, want ONE merge: reading the acting role BEFORE the agent makes the dispatching coordinator an implementer of everything and disqualifies it from reviewing (#1950 F4). reason=%q",
			decision, len(gh.merges), decision.Reason.Render())
	}
}

// TestPolicyMergeGateReachesIndependenceForARoleAuthoredApproval covers ruling
// 126350's third identity site. The at-head reviewer arm read job.Agent alone,
// so a ROLE-AUTHORED approval was refused as "no recorded reviewer author"
// BEFORE the independence check ran - which also means the role's own
// self-approval was never tested for. The arms below drive both directions, so a
// fix that merely stopped refusing such rows would fail the self-approval arm.
func TestPolicyMergeGateReachesIndependenceForARoleAuthoredApproval(t *testing.T) {
	for _, tt := range []struct {
		name           string
		implementAgent string
		implementRole  string
		approvalRole   string
		wantMerge      bool
		wantReason     string
	}{
		{
			name:           "a different role approves an agent's work: independent, so it merges",
			implementAgent: "wave-impl",
			approvalRole:   "reviewer",
			wantMerge:      true,
		},
		{
			name:          "the SAME role that implemented cannot approve its own work",
			implementRole: "reviewer",
			approvalRole:  "reviewer",
			wantMerge:     false,
			// Self-approval, NOT "no recorded reviewer author": the point of the fix is
			// that the row reaches the independence check at all.
			wantReason: "the implementing agent",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: tt.implementAgent, Type: "implement"}, JobPayload{
				Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
				ActingOrgRole: tt.implementRole,
				Result:        &AgentResult{Decision: "implemented", Summary: "implemented"},
			})
			insertCompletedJob(t, store, db.Job{ID: "review-role-approval", Agent: "", Type: "review"}, JobPayload{
				Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "head123", TaskID: "task-9",
				ReviewRound:   "review-1",
				ActingOrgRole: tt.approvalRole,
				Result:        &AgentResult{Decision: "approved", Summary: "approved by a role"},
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

			decision, err := gate.Evaluate(ctx, MergeRequest{Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9"})
			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			rendered := decision.Reason.Render()
			if tt.wantMerge && (!decision.Merged || len(gh.merges) != 1) {
				t.Fatalf("decision=%+v merges=%d, want ONE merge: a role-authored approval must reach the independence check through its normalized role. reason=%q",
					decision, len(gh.merges), rendered)
			}
			if !tt.wantMerge {
				if decision.Merged || len(gh.merges) != 0 {
					t.Fatalf("decision=%+v merges=%d, want NO merge: the same role cannot approve its own implementation", decision, len(gh.merges))
				}
				if !strings.Contains(rendered, tt.wantReason) {
					t.Fatalf("reason = %q, want it to name %q; refusing the row as merely unattributed would pass a bare no-merge assertion while never reaching independence",
						rendered, tt.wantReason)
				}
				if strings.Contains(rendered, "no recorded reviewer author") {
					t.Fatalf("reason = %q, want the INDEPENDENCE refusal rather than the unattributed fallback (#1950 F4, ruling 126350)", rendered)
				}
			}
		})
	}
}

// TestPolicyMergeGateSupersedesARoleAuthoredAtHeadObjection is #1950 F4's SIXTH
// identity site, reproduced as the certifying reviewer's adversary did it.
// Current-head supersession compared Agent columns inline while every other site
// had been routed through the resolver, so an at-head objection authored by an
// acting ROLE could never be superseded by that same role's later at-head
// approval: both Agent columns are empty, the loop skipped them, and the PR
// stayed open rendering "blocking result from " with no author.
//
// The role is deliberately written " ReVieWer " with padding and mixed case,
// because NormalizeActingOrgRole trims and lowercases and the two rows must
// resolve to ONE identity. A fix that compared raw role strings would fail here.
func TestPolicyMergeGateSupersedesARoleAuthoredAtHeadObjection(t *testing.T) {
	for _, tt := range []struct {
		name        string
		addApproval bool
		wantMerge   bool
	}{
		{name: "objection alone blocks and NAMES the role", addApproval: false, wantMerge: false},
		{name: "a later at-head approval from the same normalized role supersedes it", addApproval: true, wantMerge: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			insertCompletedJob(t, store, db.Job{ID: "implement-job", Agent: "wave-impl", Type: "implement"}, JobPayload{
				Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
				Result: &AgentResult{Decision: "implemented", Summary: "implemented"},
			})
			insertCompletedJob(t, store, db.Job{ID: "a-review-objection", Agent: "", Type: "review"}, JobPayload{
				Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "head123", TaskID: "task-9",
				ReviewRound:   "review-1",
				ActingOrgRole: " ReVieWer ",
				Result:        &AgentResult{Decision: "changes_requested", Severity: reviewseverity.P1, Summary: "role objection at head"},
			})
			if tt.addApproval {
				insertCompletedJob(t, store, db.Job{ID: "b-review-approval", Agent: "", Type: "review"}, JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 9, HeadSHA: "head123", TaskID: "task-9",
					ReviewRound:   "review-2",
					ActingOrgRole: "reviewer",
					Result:        &AgentResult{Decision: "approved", Summary: "same role, later round"},
				})
			}
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
			rendered := decision.Reason.Render()
			if tt.wantMerge && (!decision.Merged || len(gh.merges) != 1) {
				t.Fatalf("decision=%+v merges=%d, want ONE merge: the same normalized role's later at-head approval must supersede its objection (#1950 F4 site 6). reason=%q",
					decision, len(gh.merges), rendered)
			}
			if !tt.wantMerge {
				if decision.Merged || len(gh.merges) != 0 {
					t.Fatalf("decision=%+v merges=%d, want NO merge: an at-head role objection still blocks", decision, len(gh.merges))
				}
				if !strings.Contains(rendered, "reviewer") {
					t.Fatalf("reason = %q, want the normalized ROLE named; rendering \"blocking result from \" with no author is the defect this pins", rendered)
				}
			}
		})
	}
}

// TestPolicyMergeGateRefusesWhenAStaleHeadApprovalWouldRetireASessionObjection
// is #1950 F5 as the certifier reproduced it, and it is a MERGE-INTEGRITY
// regression rather than a rendering one: at 5423928d this shape MERGED and
// called the external merge once in each of three runs.
//
// The shape: a production session objection carrying neither head nor round; a
// later approval from the SAME reviewer that is also roundless but names a STALE
// head; and an independent approval at the evaluated head so nothing else blocks.
// Both objection and stale approval being roundless, reviewRoundKeyForJob falls
// back to timestamps, so the stale approval won recency and retired an objection
// it never spoke for.
//
// It enters through Evaluate and asserts ZERO external merge calls, because a
// helper-level assertion cannot observe the merge this bug performs.
func TestPolicyMergeGateRefusesWhenAStaleHeadApprovalWouldRetireASessionObjection(t *testing.T) {
	ctx := context.Background()
	store, gh, gate, request := newMergeGateQuorumScenario(t)
	// Independent approval AT the evaluated head: the gate is otherwise clean.
	insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
		id: "review-approval", agent: "audit", decision: "approved", hasResult: true,
	})
	// The objection: production session writers, so neither head nor round.
	openSessionReviewObjection(t, store, "session-objection", "session-reviewer", "", "changes_requested", reviewseverity.P1)
	setMergeGateJobTimestamps(t, store, "session-objection", "2026-09-01T10:00:00Z")
	// The stale-head approval: SAME reviewer, roundless, naming a DIFFERENT head,
	// recorded LATER so it wins the timestamp fallback.
	insertCompletedJob(t, store, db.Job{ID: "stale-head-approval", Agent: "session-reviewer", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
		HeadSHA: "staleheadsha",
		Result:  &AgentResult{Decision: "approved", Summary: "approved an older head"},
	})
	setMergeGateJobTimestamps(t, store, "stale-head-approval", "2026-09-01T18:00:00Z")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
		State: string(TaskReadyToMerge),
	}); err != nil {
		t.Fatal(err)
	}
	request.Reviewer = "audit"
	request.ExpectedTaskState = string(TaskReadyToMerge)

	decision, err := gate.Evaluate(ctx, request)
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if decision.Merged || len(gh.merges) != 0 {
		t.Fatalf("decision=%+v merges=%d, want NO merge and ZERO external merge calls: an approval naming %q cannot retire an objection about head %q (#1950 F5)",
			decision, len(gh.merges), "staleheadsha", "head123")
	}
	if task, taskErr := store.GetTask(ctx, "task-9"); taskErr != nil || task.State != string(TaskReadyToMerge) {
		t.Fatalf("task=%+v err=%v, want the task state unclaimed and unchanged", task, taskErr)
	}
}

// TestPolicyMergeGateRefusesNonVerdictCandidatesRetiringAnObjection is #1950 F6.
// The candidate loop checked head, identity, decision spelling and recency but
// never whether the candidate was a VERDICT AT ALL, so a later blocked review,
// failed review, or approved fan-out announcement from the same reviewer retired
// a live changes_requested session objection - and each of those rows is then
// itself absent from the blocking population, so an independent at-head approval
// merged.
//
// The objection is built through the production session writers, because that is
// what caught F6; a helper-level assertion cannot observe the merge.
//
// The last two arms are PROBES rather than reported findings: a delegation-child
// candidate and an engine-inserted roundless remnant both satisfy the measured
// floor (succeeded, non-fan-out, headless), so they are executed here to find out
// whether the floor is sufficient or whether those kinds need admitting clauses
// of their own. Whatever they show is reported as measured, not asserted.
func TestPolicyMergeGateRefusesNonVerdictCandidatesRetiringAnObjection(t *testing.T) {
	for _, tt := range []struct {
		name string
		seed func(t *testing.T, store *db.Store)
	}{
		{
			name: "a later BLOCKED review from the same reviewer is not a verdict",
			seed: func(t *testing.T, store *db.Store) {
				seedHeadlessCandidate(t, store, "later-blocked", "session-reviewer", JobBlocked, "approved", nil)
			},
		},
		{
			name: "a later FAILED review from the same reviewer is not a verdict",
			seed: func(t *testing.T, store *db.Store) {
				seedHeadlessCandidate(t, store, "later-failed", "session-reviewer", JobFailed, "approved", nil)
			},
		},
		{
			name: "a later APPROVED FAN-OUT announcement is a dispatch record, not a verdict",
			seed: func(t *testing.T, store *db.Store) {
				seedHeadlessCandidate(t, store, "later-fanout", "session-reviewer", JobSucceeded, "approved",
					[]Delegation{{ID: "lens-a", Agent: "lens-a", Action: "review", Prompt: "look"}})
			},
		},
		{
			name: "a delegation-child candidate is already refused by round ordering (probe needed NO clause)",
			seed: func(t *testing.T, store *db.Store) {
				encoded, err := marshalPayload(JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", ReviewRound: "review-2",
					Result: &AgentResult{Decision: "approved", Summary: "child approval"},
				})
				if err != nil {
					t.Fatalf("marshalPayload: %v", err)
				}
				if err := store.CreateJobWithEvent(context.Background(), db.Job{
					ID: "later-child", Agent: "session-reviewer", Type: "review",
					State: string(JobSucceeded), Payload: encoded,
					ParentJobID: "implement-job", DelegationID: "verify",
				}, db.JobEvent{Kind: string(JobSucceeded), Message: "child"}); err != nil {
					t.Fatalf("CreateJobWithEvent: %v", err)
				}
				setMergeGateJobTimestamps(t, store, "later-child", "2026-09-01T18:00:00Z")
			},
		},
		{
			name: "an engine-inserted roundless remnant is not attributable (probe MERGED before the clause)",
			seed: func(t *testing.T, store *db.Store) {
				insertCompletedJob(t, store, db.Job{ID: "later-remnant", Agent: "session-reviewer", Type: "review"}, JobPayload{
					Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
					Result: &AgentResult{Decision: "approved", Summary: "roundless remnant"},
				})
				setMergeGateJobTimestamps(t, store, "later-remnant", "2026-09-01T18:00:00Z")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: "review-approval", agent: "audit", decision: "approved", hasResult: true,
			})
			openSessionReviewObjection(t, store, "session-objection", "session-reviewer", "", "changes_requested", reviewseverity.P1)
			setMergeGateJobTimestamps(t, store, "session-objection", "2026-09-01T10:00:00Z")
			tt.seed(t, store)
			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9",
				State: string(TaskReadyToMerge),
			}); err != nil {
				t.Fatal(err)
			}
			request.Reviewer = "audit"
			request.ExpectedTaskState = string(TaskReadyToMerge)

			decision, err := gate.Evaluate(ctx, request)
			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if decision.Merged || len(gh.merges) != 0 {
				t.Fatalf("decision=%+v merges=%d, want NO merge and ZERO external merge calls: this candidate is not an authoritative verdict and must not retire a live objection (#1950 F6)",
					decision, len(gh.merges))
			}
			if task, taskErr := store.GetTask(ctx, "task-9"); taskErr != nil || task.State != string(TaskReadyToMerge) {
				t.Fatalf("task=%+v err=%v, want the task state unclaimed", task, taskErr)
			}
		})
	}
}

// seedHeadlessCandidate inserts a supersession candidate in the ONLY shape that
// can actually reach the state and fan-out clauses, which took two wrong fixtures
// to find and both wrong versions PASSED at the previous head:
//
//   - HEADLESS. A candidate carrying the evaluated head enters reviewsAtHead,
//     where a blocked or failed row is refused as a CRASHED REVIEWER - a different
//     guard entirely.
//   - ROUNDLESS. The objection is a session row with no round, and reviewRoundKey
//     deliberately refuses to order an explicit round against an empty one, so a
//     candidate carrying a round supersedes NOTHING and the arm proves nothing.
//   - EXTERNALLY DRIVEN, via CreateExternallyDrivenJobWithEvent, because roundless
//     alone is refused by the attributable-provenance clause. This is what leaves
//     recency as the only remaining discriminator, so state and fan-out are the
//     properties actually under test.
func seedHeadlessCandidate(t *testing.T, store *db.Store, id, agent string, state JobState, decision string, delegations []Delegation) {
	t.Helper()
	encoded, err := marshalPayload(JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
		Result: &AgentResult{Decision: decision, Summary: "later candidate", Delegations: delegations},
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.CreateExternallyDrivenJobWithEvent(context.Background(), db.Job{
		ID: id, Agent: agent, Type: "review", State: string(state), Payload: encoded,
	}, db.JobEvent{Kind: string(state), Message: "candidate"}); err != nil {
		t.Fatalf("CreateExternallyDrivenJobWithEvent(%s): %v", id, err)
	}
	setMergeGateJobTimestamps(t, store, id, "2026-09-01T18:00:00Z")
}

// seedProductionReviewRow drives a review through the PRODUCTION writers -
// Mailbox.Enqueue then finishWithPayload - rather than assembling a row. That is
// load-bearing for both arms below: the whole of #1950 F5 is that a CLI review
// carries a HeadSHA and NEITHER a round nor ExternallyDriven, and a hand-built
// fixture is free to contradict that. This one cannot.
func seedProductionReviewRow(t *testing.T, store *db.Store, id, agent, decision, headSHA string, delegations []Delegation) db.Job {
	t.Helper()
	ctx := context.Background()
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: id, Agent: agent, Action: "review", Repo: "gitmoot/gitmoot", Branch: "task-9",
		TaskID: "task-9", PullRequest: 9, HeadSHA: headSHA, Instructions: "review",
		SkipNativeReviewFanout: true,
	}); err != nil {
		t.Fatalf("Enqueue(%s) returned error: %v", id, err)
	}
	// finishWithPayload only accepts a RUNNING job, exactly as the worker leaves it
	// after claiming. Skipping this made both arms fail on the FIXTURE rather than
	// on the behaviour under test, which is how the first run of this test failed -
	// a green or red arm that never reached the predicate proves nothing.
	if _, err := store.TransitionJobState(ctx, id, string(JobQueued), string(JobRunning)); err != nil {
		t.Fatalf("TransitionJobState(%s) queued->running: %v", id, err)
	}
	queued, err := store.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", id, err)
	}
	payload, err := unmarshalPayload(queued.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(%s): %v", id, err)
	}
	payload.Result = &AgentResult{Decision: decision, Summary: "production review", Delegations: delegations}
	if decision == "changes_requested" {
		payload.Result.Severity = reviewseverity.P1
	}
	if err := mailbox.finishWithPayload(ctx, id, JobSucceeded, "job succeeded", payload); err != nil {
		t.Fatalf("finishWithPayload(%s): %v", id, err)
	}
	stored, err := store.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("GetJob(%s) after finish: %v", id, err)
	}
	return stored
}

// TestPolicyMergeGateAdmitsCLIReviewsAndRefusesAtHeadFanOut covers BOTH P1s the
// exact-head review of b22667017849b0a460026a68f9bf18d9a89ccd26 found, and they
// point in OPPOSITE directions - which is why they are pinned together and why
// each has its own mutant:
//
//   - F5, OVER-BLOCKING: the provenance clause demanded external origin or a round
//     of every candidate, and a CLI review has neither, so a legitimate succeeded
//     approval at the evaluated head could not retire an objection and the PR
//     deadlocked. Provenance is now asked only of a HEADLESS row.
//   - F6, UNDER-BLOCKING: default-deny had been added to the headless loop only,
//     while the at-head loop still tested a bare Result != nil, so an approved
//     FAN-OUT announcement at the evaluated head retired a real objection and the
//     PR merged. Both loops now share the predicate - two call sites, asserted
//     below as a COUNT, because the previous round claimed that property with one.
func TestPolicyMergeGateAdmitsCLIReviewsAndRefusesAtHeadFanOut(t *testing.T) {
	t.Run("a CLI approval at the evaluated head RETIRES a session objection (F5: must MERGE)", func(t *testing.T) {
		ctx := context.Background()
		store, gh, gate, request := newMergeGateQuorumScenario(t)
		insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
			id: "review-approval", agent: "audit", decision: "approved", hasResult: true,
		})
		mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
		// #2004: the gate resolves a runtime family for this reviewer.
		seedMergeGateFixtureAgent(t, store, "returning-reviewer")
		if _, err := mailbox.OpenExternalJob(ctx, JobRequest{
			ID: "cli-session-objection", Agent: "returning-reviewer", Action: "review",
			Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", Sender: "session",
		}); err != nil {
			t.Fatalf("OpenExternalJob returned error: %v", err)
		}
		if _, err := mailbox.CloseExternalJobWithUsage(ctx, "cli-session-objection", AgentResult{
			Decision: "changes_requested", Severity: reviewseverity.P1, Summary: "session objection",
		}, 0, "", "", ExternalJobUsage{}); err != nil {
			t.Fatalf("CloseExternalJobWithUsage returned error: %v", err)
		}
		setMergeGateJobTimestamps(t, store, "cli-session-objection", "2026-09-01T12:00:00Z")

		cli := seedProductionReviewRow(t, store, "cli-review-approval", "returning-reviewer", "approved", "head123", nil)
		setMergeGateJobTimestamps(t, store, "cli-review-approval", "2026-09-01T18:00:00Z")

		// Assert the shape rather than trust it: if a CLI review ever starts
		// carrying a round or an external origin, this arm stops reproducing F5 and
		// must fail loudly instead of passing for the wrong reason.
		cliPayload, err := unmarshalPayload(cli.Payload)
		if err != nil {
			t.Fatalf("unmarshalPayload: %v", err)
		}
		if strings.TrimSpace(cliPayload.HeadSHA) != "head123" {
			t.Fatalf("CLI review head=%q, want head123; fixture no longer reproduces F5", cliPayload.HeadSHA)
		}
		if strings.TrimSpace(cliPayload.ReviewRound) != "" || cli.ExternallyDriven {
			t.Fatalf("CLI review round=%q externally_driven=%v, want empty/false; fixture no longer reproduces F5",
				cliPayload.ReviewRound, cli.ExternallyDriven)
		}

		if err := store.UpsertTask(ctx, db.Task{
			ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9", State: string(TaskReadyToMerge),
		}); err != nil {
			t.Fatal(err)
		}
		request.Reviewer = "audit"
		request.ExpectedTaskState = string(TaskReadyToMerge)

		decision, evalErr := gate.Evaluate(ctx, request)
		if evalErr != nil {
			t.Fatalf("Evaluate returned error: %v", evalErr)
		}
		if !decision.Merged || len(gh.merges) != 1 {
			t.Fatalf("decision=%+v merges=%d, want ONE merge: a succeeded CLI approval at the evaluated head is a real verdict and must retire that reviewer's own earlier objection (#1950 F5)",
				decision, len(gh.merges))
		}
	})

	t.Run("an at-head approved FAN-OUT does NOT retire an at-head objection (F6: must BLOCK)", func(t *testing.T) {
		ctx := context.Background()
		store, gh, gate, request := newMergeGateQuorumScenario(t)
		insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
			id: "review-approval", agent: "audit", decision: "approved", hasResult: true,
		})
		seedProductionReviewRow(t, store, "at-head-objection", "fanout-reviewer", "changes_requested", "head123", nil)
		setMergeGateJobTimestamps(t, store, "at-head-objection", "2026-09-01T12:00:00Z")
		seedProductionReviewRow(t, store, "at-head-fanout", "fanout-reviewer", "approved", "head123",
			[]Delegation{{ID: "d1", Agent: "specialist", Action: "review", Prompt: "look again"}})
		setMergeGateJobTimestamps(t, store, "at-head-fanout", "2026-09-01T18:00:00Z")

		if err := store.UpsertTask(ctx, db.Task{
			ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9", State: string(TaskReadyToMerge),
		}); err != nil {
			t.Fatal(err)
		}
		request.Reviewer = "audit"
		request.ExpectedTaskState = string(TaskReadyToMerge)

		decision, evalErr := gate.Evaluate(ctx, request)
		if evalErr != nil {
			t.Fatalf("Evaluate returned error: %v", evalErr)
		}
		if decision.Merged || len(gh.merges) != 0 {
			t.Fatalf("decision=%+v merges=%d, want NO merge: an approved fan-out is a dispatch announcement, not a verdict, so it cannot retire that reviewer's own at-head objection (#1950 F6)",
				decision, len(gh.merges))
		}
		task, taskErr := store.GetTask(ctx, "task-9")
		if taskErr != nil {
			t.Fatalf("GetTask: %v", taskErr)
		}
		if task.State == string(TaskMerged) {
			t.Fatalf("task state = %q, want it NOT merged", task.State)
		}
	})

	t.Run("all THREE populations share the predicate (static census, not a claim)", func(t *testing.T) {
		source, err := os.ReadFile("merge_gate.go")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		// THE COUNT IS THE POINT. The previous round advertised "the same predicate
		// governs the objection side" while the helper had ONE call site, and a
		// one-line grep would have falsified it. A count fails; a sentence does not.
		//
		// Three populations decide whether a row may retire or supersede an
		// objection: the at-head candidate loop, the headless candidate loop, and
		// the headless objection scan. Two further objection-side sites MUST differ
		// and are excluded on stated mechanism, not preference:
		//
		//   - the at-head BLOCKING scan is preceded by TWO guards which between them
		//     let NOTHING non-succeeded reach a merge - measured per state, not
		//     argued. An earlier version of this comment also called that routing
		//     DEAD CODE; the blocked-state control DISPROVES it, because blocked
		//     does reach that scan (and is refused there). The exemption now rests
		//     only on the measured outcome, not on reachability. THEY ARE NOT ONE
		//     MECHANISM, and the first version wrongly credited one guard with all
		//     of it:
		//     the crashed-reviewer switch takes queued/running (pending) and
		//     failed/cancelled (error), while BLOCKED has no case there and is
		//     taken by the unusable-state guard in the slot scan. Deleting the
		//     switch makes all five states fall through to that guard and still be
		//     refused - defence in depth, measured, and now pinned per state with
		//     its reason by
		//     TestPolicyMergeGateAtHeadStateGuardAdmitsNothingUnsucceeded.
		//   - the two slot scans decide whether a reviewer's SLOT is filled - a
		//     fan-out with dispatched children is judged through the children -
		//     so routing them would change quorum rather than tighten it.
		guards := strings.Count(string(source), "!reviewRowIsVerdictAboutHead(") +
			strings.Count(string(source), "!reviewRowCanRetireAnObjection(")
		if guards != 3 {
			t.Fatalf("shared-predicate guard call sites = %d, want 3 (at-head candidate, headless candidate, headless objection): if a fourth population appeared it must either route through the shared core or be excluded here with its mechanism (#1950 P1-B)", guards)
		}
	})

	t.Run("ASYMMETRY: an externally-driven ROUNDLESS DELEGATION CHILD must not retire (#1950 F6)", func(t *testing.T) {
		ctx := context.Background()
		store, gh, gate, request := newMergeGateQuorumScenario(t)
		insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
			id: "review-approval", agent: "audit", decision: "approved", hasResult: true,
		})
		mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
		// #2004: the gate resolves a runtime family for this reviewer.
		seedMergeGateFixtureAgent(t, store, "asym-reviewer")
		if _, err := mailbox.OpenExternalJob(ctx, JobRequest{
			ID: "asym-objection", Agent: "asym-reviewer", Action: "review",
			Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", Sender: "session",
		}); err != nil {
			t.Fatalf("OpenExternalJob returned error: %v", err)
		}
		if _, err := mailbox.CloseExternalJobWithUsage(ctx, "asym-objection", AgentResult{
			Decision: "changes_requested", Severity: reviewseverity.P1, Summary: "session objection",
		}, 0, "", "", ExternalJobUsage{}); err != nil {
			t.Fatalf("CloseExternalJobWithUsage returned error: %v", err)
		}
		setMergeGateJobTimestamps(t, store, "asym-objection", "2026-09-01T12:00:00Z")

		// THE SHAPE IS THE WHOLE TEST, and the version this replaces got it wrong.
		// It was NAMED "asym-delegation-child" while building a row that was
		// non-external, carried ReviewRound review-2, and set NEITHER linkage field
		// - so it was refused by round ordering and proved nothing about delegation
		// children. A misnamed fixture reads as coverage to every future reader.
		// This one is what the finding describes: externally driven (provenance
		// passes), ROUNDLESS (timestamps order it, so round ordering cannot save
		// us), and carrying BOTH ParentJobID and DelegationID (so the objection
		// scan excludes it at merge_gate.go:1060). Admitted as a candidate and
		// absent as an objection is how it cleared a block it could never impose.
		encoded, err := marshalPayload(JobPayload{
			Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
			Result: &AgentResult{Decision: "approved", Summary: "delegation child verdict"},
		})
		if err != nil {
			t.Fatalf("marshalPayload: %v", err)
		}
		if err := store.CreateExternallyDrivenJobWithEvent(ctx, db.Job{
			ID: "asym-delegation-child", Agent: "asym-reviewer", Type: "review",
			State: string(JobSucceeded), Payload: encoded,
			ParentJobID: "asym-parent", DelegationID: "asym-delegation",
		}, db.JobEvent{Kind: string(JobSucceeded), Message: "delegation child"}); err != nil {
			t.Fatalf("CreateExternallyDrivenJobWithEvent: %v", err)
		}
		setMergeGateJobTimestamps(t, store, "asym-delegation-child", "2026-09-01T18:00:00Z")
		job, err := store.GetJob(ctx, "asym-delegation-child")
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		payload, err := unmarshalPayload(job.Payload)
		if err != nil {
			t.Fatalf("unmarshalPayload: %v", err)
		}
		// Assert the fixture really is the finding's shape before asserting
		// behaviour, so this cannot pass for the wrong reason its predecessor did.
		if !job.ExternallyDriven || strings.TrimSpace(payload.ReviewRound) != "" || !isDelegationChild(job) {
			t.Fatalf("fixture is externally_driven=%v round=%q delegation_child=%v; want true/empty/true or it no longer reproduces #1950 F6",
				job.ExternallyDriven, payload.ReviewRound, isDelegationChild(job))
		}
		if reviewRowIsVerdictAboutHead(job, payload, "head123") {
			t.Fatalf("shared predicate ADMITTED an externally-driven roundless delegation child as a superseding candidate while the objection scan EXCLUDES that same row: it can clear a block it could never impose (#1950 F6)")
		}

		if err := store.UpsertTask(ctx, db.Task{
			ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9", State: string(TaskReadyToMerge),
		}); err != nil {
			t.Fatal(err)
		}
		request.Reviewer = "audit"
		request.ExpectedTaskState = string(TaskReadyToMerge)

		decision, evalErr := gate.Evaluate(ctx, request)
		if evalErr != nil {
			t.Fatalf("Evaluate returned error: %v", evalErr)
		}
		if decision.Merged || len(gh.merges) != 0 {
			t.Fatalf("decision=%+v merges=%d, want ZERO external merge calls: a delegation child is accounted through its parent's fan-out evidence and must not retire a session objection (#1950 F6)", decision, len(gh.merges))
		}
		task, taskErr := store.GetTask(ctx, "task-9")
		if taskErr != nil {
			t.Fatalf("GetTask: %v", taskErr)
		}
		if task.State != string(TaskReadyToMerge) {
			t.Fatalf("task state = %q, want it left UNCLAIMED at ready_to_merge (#1950 F6)", task.State)
		}
	})
}

// seedMatrixCandidate inserts a candidate with EXACTLY the head/provenance
// combination named, so each cell of the four-cell matrix is explicit in the
// fixture rather than implied by a helper's defaults.
func seedMatrixCandidate(t *testing.T, store *db.Store, id, agent, headSHA, round string, externallyDriven bool) {
	t.Helper()
	encoded, err := marshalPayload(JobPayload{
		Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
		HeadSHA: headSHA, ReviewRound: round,
		Result: &AgentResult{Decision: "approved", Summary: "matrix candidate"},
	})
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	job := db.Job{ID: id, Agent: agent, Type: "review", State: string(JobSucceeded), Payload: encoded}
	event := db.JobEvent{Kind: string(JobSucceeded), Message: "matrix candidate"}
	if externallyDriven {
		if err := store.CreateExternallyDrivenJobWithEvent(context.Background(), job, event); err != nil {
			t.Fatalf("CreateExternallyDrivenJobWithEvent(%s): %v", id, err)
		}
	} else if err := store.CreateJobWithEvent(context.Background(), job, event); err != nil {
		t.Fatalf("CreateJobWithEvent(%s): %v", id, err)
	}
	setMergeGateJobTimestamps(t, store, id, "2026-09-01T18:00:00Z")
}

// TestPolicyMergeGateProvenanceScopeMatrix measures ALL FOUR cells of the
// head/provenance matrix SIMULTANEOUSLY at one head. F5's fix is a scope
// NARROWING - provenance applies only to headless rows - and this campaign's
// signature failure is trading one cell for another: round 7 traded stale-head
// admission for a bypass, round 9 traded remnant exclusion for the CLI deadlock.
// Three cells plus an argument is what allowed both. The table is the argument.
func TestPolicyMergeGateProvenanceScopeMatrix(t *testing.T) {
	for _, tt := range []struct {
		name        string
		headSHA     string
		round       string
		extDriven   bool
		wantRetired bool
		why         string
	}{
		{
			name: "cell 1: headless WITHOUT provenance must NOT retire", headSHA: "", round: "", extDriven: false, wantRetired: false,
			why: "the unattributable engine-inserted remnant; admitting it retires a live objection and then vanishes from the blocking population",
		},
		{
			name: "cell 2: headless WITH provenance MUST retire", headSHA: "", round: "", extDriven: true, wantRetired: true,
			why: "a session-written approval is a real verdict; refusing it would deadlock every session re-review",
		},
		{
			name: "cell 3: head-bearing MATCHING without provenance MUST retire", headSHA: "head123", round: "", extDriven: false, wantRetired: true,
			why: "the CLI shape - head always, round never, never externally driven. This is the #1950 P1-A deadlock",
		},
		{
			name: "cell 4: head-bearing NON-matching must NOT retire", headSHA: "stale999", round: "", extDriven: false, wantRetired: false,
			why: "a verdict about a different commit cannot speak for this head",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: "review-approval", agent: "audit", decision: "approved", hasResult: true,
			})
			mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
			// #2004: the gate resolves a runtime family for this reviewer.
			seedMergeGateFixtureAgent(t, store, "matrix-reviewer")
			if _, err := mailbox.OpenExternalJob(ctx, JobRequest{
				ID: "matrix-objection", Agent: "matrix-reviewer", Action: "review",
				Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", Sender: "session",
			}); err != nil {
				t.Fatalf("OpenExternalJob returned error: %v", err)
			}
			if _, err := mailbox.CloseExternalJobWithUsage(ctx, "matrix-objection", AgentResult{
				Decision: "changes_requested", Severity: reviewseverity.P1, Summary: "session objection",
			}, 0, "", "", ExternalJobUsage{}); err != nil {
				t.Fatalf("CloseExternalJobWithUsage returned error: %v", err)
			}
			setMergeGateJobTimestamps(t, store, "matrix-objection", "2026-09-01T12:00:00Z")

			seedMatrixCandidate(t, store, "matrix-candidate", "matrix-reviewer", tt.headSHA, tt.round, tt.extDriven)

			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9", State: string(TaskReadyToMerge),
			}); err != nil {
				t.Fatal(err)
			}
			request.Reviewer = "audit"
			request.ExpectedTaskState = string(TaskReadyToMerge)

			decision, evalErr := gate.Evaluate(ctx, request)
			if evalErr != nil {
				t.Fatalf("Evaluate returned error: %v", evalErr)
			}
			if tt.wantRetired && (!decision.Merged || len(gh.merges) != 1) {
				t.Fatalf("decision=%+v merges=%d, want the objection RETIRED (one merge): %s", decision, len(gh.merges), tt.why)
			}
			if !tt.wantRetired && (decision.Merged || len(gh.merges) != 0) {
				t.Fatalf("decision=%+v merges=%d, want the objection to SURVIVE (no merge): %s", decision, len(gh.merges), tt.why)
			}
		})
	}
}

// TestPolicyMergeGateAtHeadStateGuardAdmitsNothingUnsucceeded drives EVERY
// non-succeeded job state through Evaluate at the evaluated head and asserts
// WHICH mechanism refuses it.
//
// This is the LOAD-BEARING ASSUMPTION behind not routing the at-head blocking
// scan through the shared predicate. Asserting only "no merge" does not test
// it: the FIRST version of this test asserted exactly that, passed on all five
// states, AND STILL PASSED ON ALL FIVE WITH THE CRASHED-REVIEWER GUARD DELETED.
// It was measuring a backstop, not the guard. Pinning the reason string is what
// makes each state's refusal attributable to a named mechanism.
//
// TWO guards precede the scan, which is the correction this test encodes:
//
//   - the crashed-reviewer switch: queued/running -> PENDING, failed/cancelled
//     -> a crashed-reviewer error.
//   - the unusable-state guard in the slot scan (merge_gate.go:1200): BLOCKED.
//     The switch has no JobBlocked case, so attributing blocked to it - as an
//     earlier version of the census comment did - is wrong.
//
// Measured: with the crashed-reviewer switch deleted, all five fall through to
// the unusable-state guard and are still refused. Defence in depth, so no
// bypass - but the two mechanisms are distinct and are named separately.
func TestPolicyMergeGateAtHeadStateGuardAdmitsNothingUnsucceeded(t *testing.T) {
	for _, tt := range []struct {
		state      JobState
		wantReason string
		mechanism  string
	}{
		{JobQueued, "waiting for reviewer", "crashed-reviewer switch, pending arm"},
		{JobRunning, "waiting for reviewer", "crashed-reviewer switch, pending arm"},
		{JobFailed, "crashed reviewer", "crashed-reviewer switch, error arm"},
		{JobCancelled, "crashed reviewer", "crashed-reviewer switch, error arm"},
		{JobBlocked, "has unusable job state", "unusable-state guard in the slot scan, NOT the switch"},
	} {
		t.Run(string(tt.state), func(t *testing.T) {
			ctx := context.Background()
			store, gh, gate, request := newMergeGateQuorumScenario(t)
			insertMergeGateReviewFixture(t, store, mergeGateReviewFixture{
				id: "review-approval", agent: "audit", decision: "approved", hasResult: true,
			})
			// An APPROVED result in a non-succeeded state is the dangerous
			// direction: a row that could be mistaken for a verdict satisfying the
			// gate. It carries the evaluated head so it lands in the at-head
			// population these guards are responsible for.
			encoded, err := marshalPayload(JobPayload{
				Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9", HeadSHA: "head123",
				Result: &AgentResult{Decision: "approved", Summary: "unsucceeded at-head row"},
			})
			if err != nil {
				t.Fatalf("marshalPayload: %v", err)
			}
			if err := store.CreateJobWithEvent(ctx, db.Job{
				ID: "unsucceeded-at-head", Agent: "state-reviewer", Type: "review",
				State: string(tt.state), Payload: encoded,
			}, db.JobEvent{Kind: string(tt.state), Message: "at-head row"}); err != nil {
				t.Fatalf("CreateJobWithEvent: %v", err)
			}
			setMergeGateJobTimestamps(t, store, "unsucceeded-at-head", "2026-09-01T18:00:00Z")

			if err := store.UpsertTask(ctx, db.Task{
				ID: "task-9", RepoFullName: "gitmoot/gitmoot", Branch: "task-9", State: string(TaskReadyToMerge),
			}); err != nil {
				t.Fatal(err)
			}
			request.Reviewer = "audit"
			request.ExpectedTaskState = string(TaskReadyToMerge)

			decision, evalErr := gate.Evaluate(ctx, request)
			if evalErr != nil {
				t.Fatalf("Evaluate returned error: %v", evalErr)
			}
			if decision.Merged || len(gh.merges) != 0 {
				t.Fatalf("state %s reached a MERGE: decision=%+v merges=%d. The at-head blocking scan is exempt from the shared predicate ONLY because these guards admit nothing non-succeeded; this state slipped past, so that exemption is a live bypass (#1950 arm 2)",
					tt.state, decision, len(gh.merges))
			}
			if rendered := decision.Reason.Render(); !strings.Contains(rendered, tt.wantReason) {
				t.Fatalf("state %s refused with reason %q, want it to contain %q (%s). A refusal by the WRONG mechanism means the guard this exemption cites is not the one doing the work (#1950 arm 2)",
					tt.state, rendered, tt.wantReason, tt.mechanism)
			}
		})
	}
}

// TestReviewRowSupersedingImpliesObjectable is the invariant directive 127201
// names: A ROW THAT SUPERSEDES MUST BE A ROW THAT CAN OBJECT.
//
// #1950 F6 was neither predicate being wrong on its own - it was the two
// disagreeing about the SAME row. An externally driven, roundless delegation
// child was admitted as a superseding candidate and simultaneously excluded
// from the objection population, so it cleared a block it could never impose.
// Fixing that one shape leaves the NEXT disagreement free to appear, which is
// how rounds 5, 7, 8, 9 and 10 each found a new one.
//
// So this enumerates the cross product of every field either side reads and
// asserts CONTAINMENT: admitted-as-candidate implies not-excluded-as-objection.
// A future clause added to one side and not the other fails here by
// construction, without anyone having to think of the shape first.
func TestReviewRowSupersedingImpliesObjectable(t *testing.T) {
	const headSHA = "head123"
	heads := []string{"", headSHA, "stale999"}
	rounds := []string{"", "review-2"}
	bools := []bool{false, true}
	states := []JobState{JobSucceeded, JobBlocked, JobFailed}

	checked, admitted := 0, 0
	for _, head := range heads {
		for _, round := range rounds {
			for _, ext := range bools {
				for _, child := range bools {
					for _, fanOut := range bools {
						for _, state := range states {
							checked++
							result := &AgentResult{Decision: "approved", Summary: "enumerated row"}
							if fanOut {
								result.Delegations = []Delegation{{ID: "d1", Agent: "specialist", Action: "review", Prompt: "look"}}
							}
							payload := JobPayload{
								Repo: "gitmoot/gitmoot", PullRequest: 9, TaskID: "task-9",
								HeadSHA: head, ReviewRound: round, Result: result,
							}
							job := db.Job{
								ID: "enumerated", Agent: "reviewer", Type: "review",
								State: string(state), ExternallyDriven: ext,
							}
							if child {
								job.ParentJobID, job.DelegationID = "parent", "delegation"
							}
							if !reviewRowIsVerdictAboutHead(job, payload, headSHA) {
								continue
							}
							admitted++
							// Every reason the OBJECTION populations drop a row. If an
							// admitted candidate matches any of them, that row can
							// supersede an objection while being unable to be one.
							switch {
							case JobState(job.State) != JobSucceeded:
								t.Errorf("admitted candidate is not succeeded (state=%s): the objection scans skip it, so it can retire a block it cannot impose", state)
							case payload.Result == nil:
								t.Errorf("admitted candidate has no result: the objection scans skip it")
							case reviewRowIsFanOut(payload.Result):
								t.Errorf("admitted candidate is a fan-out announcement: all four objection sites skip it")
							case isDelegationChild(job):
								t.Errorf("admitted candidate is a delegation child (head=%q round=%q ext=%v): the objection scan excludes it at merge_gate.go:1060 - this is #1950 F6 exactly", head, round, ext)
							case isIntegrationWorktreeReview(payload):
								t.Errorf("admitted candidate is an integration-worktree review: the headless objection scan excludes it")
							case strings.TrimSpace(head) != "" && strings.TrimSpace(head) != headSHA:
								t.Errorf("admitted candidate carries a foreign head %q", head)
							}
						}
					}
				}
			}
		}
	}
	// Guard against the enumeration silently becoming vacuous: if a future clause
	// refuses everything, containment holds trivially and proves nothing.
	if admitted == 0 {
		t.Fatalf("enumerated %d shapes and NONE were admitted as candidates; containment then holds vacuously and this test proves nothing", checked)
	}
	t.Logf("enumerated %d shapes, %d admitted as superseding candidates, all of them objectable", checked, admitted)
}
