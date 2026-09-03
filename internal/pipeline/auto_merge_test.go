package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func settleBoundImplementStageJob(t *testing.T, store *db.Store, jobID, decision string, binding PipelineStagePRBinding) {
	t.Helper()
	ctx := context.Background()
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	payload.PullRequest = binding.PullRequest
	payload.HeadSHA = binding.HeadSHA
	payload.Branch = binding.Branch
	payload.TaskID = binding.TaskID
	payload.LeadAgent = binding.LeadAgent
	payload.Result = &workflow.AgentResult{Decision: decision, Summary: "implementation settled"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	to := jobStateForDecision(decision)
	ok, err := store.TransitionJobStatePayloadWithEvent(ctx, job.ID, job.State, to, string(encoded), db.JobEvent{JobID: job.ID, Kind: to, Message: "settled by test"})
	if err != nil || !ok {
		t.Fatalf("settle implement job: ok=%v err=%v", ok, err)
	}
}

const pipelineAutoMergeSpec = `name: auto-merge
repo: owner/repo
allow_auto_merge: true
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: review
    agent: reviewer
    prompt: Review the implementation PR.
    action: review
    source: impl
    needs: [impl]
    success_decisions: [approved]
  - id: merge
    gate: pr_merged
    merge: auto
    source: impl
    needs: [impl]
`

type stubPipelineAutoMerger struct {
	readiness    workflow.PipelineAutoMergeReadiness
	evaluateErr  error
	mergeResult  workflow.PipelineAutoMergeResult
	mergeErr     error
	evaluateReqs []workflow.PipelineAutoMergeRequest
	mergeReqs    []workflow.PipelineAutoMergeRequest
}

type claimRacePipelineAutoMerger struct {
	evaluations atomic.Int32
	mergeCalls  atomic.Int32
	merged      atomic.Bool
	bothReady   chan struct{}
}

func (s *claimRacePipelineAutoMerger) Evaluate(_ context.Context, _ workflow.PipelineAutoMergeRequest) (workflow.PipelineAutoMergeReadiness, error) {
	if s.merged.Load() {
		return workflow.PipelineAutoMergeReadiness{Merged: true, CurrentHeadSHA: "0123456789abcdef"}, nil
	}
	if s.evaluations.Add(1) == 2 {
		close(s.bothReady)
	}
	<-s.bothReady
	return workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"}, nil
}

func (s *claimRacePipelineAutoMerger) Merge(_ context.Context, _ workflow.PipelineAutoMergeRequest) (workflow.PipelineAutoMergeResult, error) {
	s.mergeCalls.Add(1)
	s.merged.Store(true)
	return workflow.PipelineAutoMergeResult{Merged: true, MergeCommitSHA: "race-merge"}, nil
}

func (s *stubPipelineAutoMerger) Evaluate(_ context.Context, request workflow.PipelineAutoMergeRequest) (workflow.PipelineAutoMergeReadiness, error) {
	s.evaluateReqs = append(s.evaluateReqs, request)
	return s.readiness, s.evaluateErr
}

func (s *stubPipelineAutoMerger) Merge(_ context.Context, request workflow.PipelineAutoMergeRequest) (workflow.PipelineAutoMergeResult, error) {
	s.mergeReqs = append(s.mergeReqs, request)
	return s.mergeResult, s.mergeErr
}

func advanceWithAutoMerge(t *testing.T, store *db.Store, enqueue PipelineStageEnqueuer, rec db.Pipeline, spec Spec, run db.PipelineRun, now time.Time, executor PipelineAutoMergeExecutor) db.PipelineRun {
	t.Helper()
	updated, err := AdvancePipelineRunWithAutoMerge(context.Background(), store, enqueue, rec, spec, run, now, executor)
	if err != nil {
		t.Fatalf("AdvancePipelineRunWithAutoMerge: %v", err)
	}
	return updated
}

func settleBoundReviewJob(t *testing.T, store *db.Store, jobID, decision, head string) {
	t.Helper()
	ctx := context.Background()
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(review): %v", err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(review): %v", err)
	}
	payload.HeadSHA = head
	payload.Result = &workflow.AgentResult{Decision: decision, Summary: "review settled"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal review payload: %v", err)
	}
	to := jobStateForDecision(decision)
	ok, err := store.TransitionJobStatePayloadWithEvent(ctx, job.ID, job.State, to, string(encoded), db.JobEvent{JobID: job.ID, Kind: to, Message: "review settled by test"})
	if err != nil || !ok {
		t.Fatalf("settle review: ok=%v err=%v", ok, err)
	}
}

func prepareAutoMergeGate(t *testing.T) (*db.Store, PipelineStageEnqueuer, db.Pipeline, Spec, db.PipelineRun, string, time.Time) {
	t.Helper()
	store := pipelineAdvanceStore(t)
	rec, spec := newTestPipeline(t, store, "auto-merge", pipelineAutoMergeSpec)
	enqueue := testStageEnqueuer(store)
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, spec, enqueue, now)
	impl := stageRow(t, store, run.ID, "impl")
	setttle := PipelineStagePRBinding{PullRequest: 813, HeadSHA: "0123456789abcdef", Branch: "feat/813", TaskID: "task-813", LeadAgent: "coder"}
	settleBoundImplementStageJob(t, store, impl.JobID, "implemented", setttle)
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(time.Second), &stubPipelineAutoMerger{})
	return store, enqueue, rec, spec, run, impl.JobID, now
}

func TestPipelineAutoMergeGateExecutesAfterApprovedReviewAndGreenChecks(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	review := stageRow(t, store, run.ID, "review")
	settleBoundReviewJob(t, store, review.JobID, "approved", "0123456789abcdef")
	executor := &stubPipelineAutoMerger{
		readiness:   workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{Merged: true, MergeCommitSHA: "merge-sha"},
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)
	if len(executor.mergeReqs) != 1 {
		t.Fatalf("merge calls = %d, want 1", len(executor.mergeReqs))
	}
	request := executor.mergeReqs[0]
	if request.Repo != "owner/repo" || request.PullRequest != 813 || request.HeadSHA != "0123456789abcdef" || request.Pipeline != "auto-merge" || request.RunID != run.ID || request.StageID != "merge" {
		t.Fatalf("merge request = %+v", request)
	}
	if run.State != RunSucceeded || stageRow(t, store, run.ID, "merge").State != StageSucceeded {
		t.Fatalf("run/gate = %+v / %+v, want succeeded", run, stageRow(t, store, run.ID, "merge"))
	}
	events, err := store.ListJobEvents(context.Background(), sourceJobID)
	if err != nil {
		t.Fatalf("ListJobEvents(source): %v", err)
	}
	intent, confirmed := -1, -1
	for i, event := range events {
		switch event.Kind {
		case "pipeline_auto_merge_claim":
			intent = i
			if !strings.Contains(event.Message, `"pull_request":813`) || !strings.Contains(event.Message, `"head_sha":"0123456789abcdef"`) || !strings.Contains(event.Message, `"run_id":"`+run.ID+`"`) {
				t.Fatalf("intent event = %+v", event)
			}
		case "pipeline_auto_merge_confirmed":
			confirmed = i
		}
	}
	if intent < 0 || confirmed <= intent {
		t.Fatalf("auto-merge event order intent=%d confirmed=%d events=%+v", intent, confirmed, events)
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(3*time.Second), executor)
	if len(executor.mergeReqs) != 1 {
		t.Fatalf("terminal rescan merge calls = %d, want 1", len(executor.mergeReqs))
	}
}

// A TRANSIENT merge-boundary refusal must keep the gate waiting. The fold turned
// any !Merged into "retry stopped", so a workload-mode reconciliation hold that
// Evaluate reports as Waiting terminally blocked the run once it appeared in the
// Evaluate->Merge window, and the at-most-once claim was already consumed
// (#1783 review, F4). The claim is why the retry is safe: the next scan cannot
// call Merge again.
func TestPipelineAutoMergeWaitingResultKeepsTheGateRetryable(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	executor := &stubPipelineAutoMerger{
		readiness: workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{
			Waiting: true,
			Reason:  "workload-mode change requires reconciliation at head 0123456 against operating-mode note 41",
		},
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)

	if len(executor.mergeReqs) != 1 {
		t.Fatalf("merge calls = %d, want exactly one attempt", len(executor.mergeReqs))
	}
	gate := stageRow(t, store, run.ID, "merge")
	if gate.State == StageBlocked || gate.State == StageFailed {
		t.Fatalf("a transient hold ended the run: gate=%+v run=%+v", gate, run)
	}
	if run.State == RunBlocked || run.State == RunFailed {
		t.Fatalf("run = %q, want it still open after a transient hold", run.State)
	}

	// The consumed claim keeps the retry at-most-once: a later scan must not
	// call Merge again, and the gate must still not be terminal.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(3*time.Second), executor)
	if len(executor.mergeReqs) != 1 {
		t.Fatalf("rescan merge calls = %d, want the claim to hold at one", len(executor.mergeReqs))
	}
	if gate := stageRow(t, store, run.ID, "merge"); gate.State == StageBlocked || gate.State == StageFailed {
		t.Fatalf("rescan ended the run: gate=%+v", gate)
	}
	events, err := store.ListJobEvents(context.Background(), sourceJobID)
	if err != nil {
		t.Fatalf("ListJobEvents(source): %v", err)
	}
	for _, event := range events {
		if event.Kind == "pipeline_auto_merge_confirmed" {
			t.Fatalf("a held merge recorded a confirmation: %+v", event)
		}
	}
}

// #1685. A review fan-out is refused at STAGE SETTLEMENT, before anything
// depends on it. That is the load-bearing layer: a succeeded stage satisfies
// pipelineStageDepsSucceeded and authorizes every dependent stage, so folding an
// announcement as success let all of them run on a review that never happened.
// The auto-merge gate is only one such dependent.
func TestPipelineFanOutReviewNeverSettlesAsStageSuccess(t *testing.T) {
	store, enqueue, rec, spec, run, _, now := prepareAutoMergeGate(t)
	review := stageRow(t, store, run.ID, "review")
	settleBoundReviewJob(t, store, review.JobID, "approved", "0123456789abcdef")
	declareBoundReviewDelegations(t, store, review.JobID)
	executor := &stubPipelineAutoMerger{
		readiness:   workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{Merged: true, MergeCommitSHA: "merge-sha"},
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)

	settledReview := stageRow(t, store, run.ID, "review")
	if settledReview.State != StageFailed {
		t.Fatalf("review stage = %q, want failed: a fan-out settled as success and can authorize dependents", settledReview.State)
	}
	if !strings.Contains(settledReview.Summary, "not a verdict") {
		t.Fatalf("review stage summary = %q, want the fan-out reason", settledReview.Summary)
	}
	if len(executor.mergeReqs) != 0 {
		t.Fatalf("merge calls = %d, want none for a fan-out review", len(executor.mergeReqs))
	}
	if gate := stageRow(t, store, run.ID, "merge"); gate.State == StageSucceeded {
		t.Fatalf("merge gate = %q, want anything but succeeded", gate.State)
	}
}

// DEFENCE IN DEPTH, pinned separately so removing either layer is caught: the
// auto-merge gate refuses a fan-out on its own, evaluated against a review stage
// row that is already SUCCEEDED. That is the shape a row settled before this fix
// still has on disk, and the shape any future settlement path that forgets the
// classification would produce.
func TestPipelineAutoMergeGateRefusesFanOutReview(t *testing.T) {
	store, enqueue, rec, spec, run, _, now := prepareAutoMergeGate(t)
	review := stageRow(t, store, run.ID, "review")
	settleBoundReviewJob(t, store, review.JobID, "approved", "0123456789abcdef")
	// Settle the STAGE ROW as succeeded first, then make its job a fan-out, so the
	// gate is reached with a dependency that already reads as satisfied.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(time.Second), &stubPipelineAutoMerger{
		readiness: workflow.PipelineAutoMergeReadiness{Ready: false, Reason: "checks pending", CurrentHeadSHA: "0123456789abcdef"},
	})
	if got := stageRow(t, store, run.ID, "review"); got.State != StageSucceeded {
		t.Fatalf("review stage = %q, want succeeded before arming the gate probe", got.State)
	}
	declareBoundReviewDelegations(t, store, review.JobID)

	executor := &stubPipelineAutoMerger{
		readiness:   workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{Merged: true, MergeCommitSHA: "merge-sha"},
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)
	if len(executor.mergeReqs) != 0 {
		t.Fatalf("merge calls = %d, want none for a fan-out review", len(executor.mergeReqs))
	}
	gate := stageRow(t, store, run.ID, "merge")
	if gate.State != StageBlocked {
		t.Fatalf("gate state = %q, want blocked", gate.State)
	}
	if !strings.Contains(gate.Summary, "not a verdict") {
		t.Fatalf("gate summary = %q, want the fan-out reason", gate.Summary)
	}
}

// declareBoundReviewDelegations rewrites a settled review job's result into the
// shape a production pipeline fan-out actually has WHEN THIS GATE READS IT.
//
// That shape has NO delegations. Mailbox.Run strips them for every
// non-orchestrate pipeline stage, and a review stage cannot set
// orchestrate:true, so a stored pipeline review payload carrying delegations is
// a shape production never writes. The earlier version of this helper wrote
// exactly that, which is why the gate's guard looked covered while a real
// coordinator announcement still auto-merged. What survives the strip is the
// FanOut classification, and that is what the gate must consume.
//
// The strip itself is pinned separately, at its own seam, by
// TestMailboxRunPreservesFanOutClassificationAcrossPipelineStrip.
func declareBoundReviewDelegations(t *testing.T, store *db.Store, jobID string) {
	t.Helper()
	ctx := context.Background()
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(review): %v", err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(review): %v", err)
	}
	payload.Result.Delegations = nil
	payload.Result.FanOut = true
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal review payload: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, jobID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload(review): %v", err)
	}
}

func TestPipelineAutoMergeGateRequiresEverySourceBoundReview(t *testing.T) {
	const secondReview = `  - id: review-two
    agent: reviewer
    prompt: Review the implementation PR again.
    action: review
    source: impl
    needs: [impl]
    success_decisions: [approved]
`
	specYAML := strings.Replace(pipelineAutoMergeSpec, "  - id: merge\n", secondReview+"  - id: merge\n", 1)
	store := pipelineAdvanceStore(t)
	rec, spec := newTestPipeline(t, store, "auto-merge", specYAML)
	enqueue := testStageEnqueuer(store)
	now := time.Date(2026, 7, 11, 8, 30, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, spec, enqueue, now)
	impl := stageRow(t, store, run.ID, "impl")
	settleBoundImplementStageJob(t, store, impl.JobID, "implemented", PipelineStagePRBinding{PullRequest: 813, HeadSHA: "all-review-head"})
	executor := &stubPipelineAutoMerger{readiness: workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "all-review-head"}, mergeResult: workflow.PipelineAutoMergeResult{Merged: true}}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(time.Second), executor)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "all-review-head")
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)
	if len(executor.evaluateReqs) != 0 || len(executor.mergeReqs) != 0 || stageRow(t, store, run.ID, "merge").State != StageRunning {
		t.Fatalf("gate advanced before every review: evaluate=%d merge=%d gate=%+v", len(executor.evaluateReqs), len(executor.mergeReqs), stageRow(t, store, run.ID, "merge"))
	}
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review-two").JobID, "approved", "all-review-head")
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(3*time.Second), executor)
	if run.State != RunSucceeded || len(executor.mergeReqs) != 1 {
		t.Fatalf("gate did not merge after every review: run=%+v merge=%d", run, len(executor.mergeReqs))
	}
}

func TestPipelineAutoMergeClaimAllowsExactlyOneRacingMerge(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	waiting := &stubPipelineAutoMerger{readiness: workflow.PipelineAutoMergeReadiness{Waiting: true, CurrentHeadSHA: "0123456789abcdef"}}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), waiting)
	gateRow := stageRow(t, store, run.ID, "merge")
	var gateStage Stage
	for _, candidate := range spec.Stages {
		if candidate.ID == "merge" {
			gateStage = candidate
			break
		}
	}
	executor := &claimRacePipelineAutoMerger{bothReady: make(chan struct{})}
	deps := pipelineStageSettleDeps{store: store, rec: rec, run: run, now: now.Add(3 * time.Second), autoMerge: executor}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, _, _, _, err := gateStageSettleOutcome(context.Background(), deps, spec, gateStage, gateRow)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("racing settle: %v", err)
		}
	}
	if got := executor.mergeCalls.Load(); got != 1 {
		t.Fatalf("racing merge calls = %d, want exactly 1", got)
	}
	events, err := store.ListJobEvents(context.Background(), sourceJobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	claims := 0
	for _, event := range events {
		if event.Kind == "pipeline_auto_merge_claim" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("claim events = %d, want 1; events=%+v", claims, events)
	}
	settled, state, _, _, _, err := gateStageSettleOutcome(context.Background(), deps, spec, gateStage, gateRow)
	if err != nil || !settled || state != StageSucceeded {
		t.Fatalf("post-race settle = settled:%v state:%q err:%v", settled, state, err)
	}
}

func TestPipelineAutoMergeGateSafetyStops(t *testing.T) {
	tests := []struct {
		name          string
		decision      string
		reviewHead    string
		mutateSpec    func(*Spec)
		readiness     workflow.PipelineAutoMergeReadiness
		mergeErr      error
		wantRunState  string
		wantGateState string
		wantNeed      string
		wantEvaluate  int
		wantMerge     int
	}{
		{name: "changes requested", decision: "changes_requested", reviewHead: "0123456789abcdef", wantRunState: RunFailed, wantGateState: StageBlocked, wantNeed: "has not approved"},
		{name: "reviewed head mismatch", decision: "approved", reviewHead: "different-head", wantRunState: RunBlocked, wantGateState: StageBlocked, wantNeed: "reviewed head", wantEvaluate: 0},
		{name: "live head drift", decision: "approved", reviewHead: "0123456789abcdef", readiness: workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "drifted-head"}, wantRunState: RunBlocked, wantGateState: StageBlocked, wantNeed: "head drifted", wantEvaluate: 1},
		{name: "checks pending", decision: "approved", reviewHead: "0123456789abcdef", readiness: workflow.PipelineAutoMergeReadiness{Waiting: true, Reason: "checks pending"}, wantRunState: RunRunning, wantGateState: StageRunning, wantEvaluate: 1},
		{name: "allow key missing defensively", decision: "approved", reviewHead: "0123456789abcdef", mutateSpec: func(spec *Spec) { spec.AllowAutoMerge = false }, wantRunState: RunBlocked, wantGateState: StageBlocked, wantNeed: "allow_auto_merge", wantEvaluate: 0},
		{name: "already merged", decision: "approved", reviewHead: "0123456789abcdef", readiness: workflow.PipelineAutoMergeReadiness{Merged: true, MergeCommitSHA: "merged"}, wantRunState: RunSucceeded, wantGateState: StageSucceeded, wantEvaluate: 1},
		{name: "merge API error", decision: "approved", reviewHead: "0123456789abcdef", readiness: workflow.PipelineAutoMergeReadiness{Ready: true}, mergeErr: errors.New("boom"), wantRunState: RunBlocked, wantGateState: StageBlocked, wantNeed: "retry stopped", wantEvaluate: 1, wantMerge: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, enqueue, rec, spec, run, _, now := prepareAutoMergeGate(t)
			review := stageRow(t, store, run.ID, "review")
			settleBoundReviewJob(t, store, review.JobID, tc.decision, tc.reviewHead)
			if tc.mutateSpec != nil {
				tc.mutateSpec(&spec)
			}
			readiness := tc.readiness
			if tc.wantEvaluate > 0 && !readiness.Merged && readiness.CurrentHeadSHA == "" {
				readiness.CurrentHeadSHA = "0123456789abcdef"
			}
			executor := &stubPipelineAutoMerger{readiness: readiness, mergeErr: tc.mergeErr}
			run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)
			gate := stageRow(t, store, run.ID, "merge")
			if run.State != tc.wantRunState || gate.State != tc.wantGateState {
				t.Fatalf("run/gate states = %s/%s, want %s/%s; run=%+v gate=%+v", run.State, gate.State, tc.wantRunState, tc.wantGateState, run, gate)
			}
			if tc.wantNeed != "" && !strings.Contains(gate.NeedsJSON, tc.wantNeed) {
				t.Fatalf("gate needs = %q, want substring %q", gate.NeedsJSON, tc.wantNeed)
			}
			if len(executor.evaluateReqs) != tc.wantEvaluate || len(executor.mergeReqs) != tc.wantMerge {
				t.Fatalf("evaluate/merge calls = %d/%d, want %d/%d", len(executor.evaluateReqs), len(executor.mergeReqs), tc.wantEvaluate, tc.wantMerge)
			}
			if tc.mergeErr != nil {
				run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(3*time.Second), executor)
				if len(executor.mergeReqs) != 1 {
					t.Fatalf("blocked rescan retried merge %d times", len(executor.mergeReqs))
				}
			}
		})
	}
}

func TestPipelineHumanMergeGateNeverTouchesAutoMergeExecutor(t *testing.T) {
	const specYAML = `name: human-merge
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix it.
    action: implement
    write: true
  - id: wait
    gate: pr_merged
    source: impl
    needs: [impl]
`
	store := pipelineAdvanceStore(t)
	rec, spec := newTestPipeline(t, store, "human-merge", specYAML)
	enqueue := testStageEnqueuer(store)
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, spec, enqueue, now)
	impl := stageRow(t, store, run.ID, "impl")
	settleBoundImplementStageJob(t, store, impl.JobID, "implemented", PipelineStagePRBinding{PullRequest: 814, HeadSHA: "human-head"})
	executor := &stubPipelineAutoMerger{}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(time.Second), executor)
	markPipelinePRMerged(t, store, "owner/repo", 814, "merged")
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)
	if run.State != RunSucceeded || len(executor.evaluateReqs) != 0 || len(executor.mergeReqs) != 0 {
		t.Fatalf("human gate run=%+v evaluate=%d merge=%d", run, len(executor.evaluateReqs), len(executor.mergeReqs))
	}
}
