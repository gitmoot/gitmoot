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

// A TRANSIENT merge-boundary refusal must keep the gate waiting AND stay
// mergeable once the condition clears. My first version of this test asserted
// the opposite and called it safety: it required mergeReqs to stay at 1 on
// rescan, which is exactly the defect the round-3 review measured - the
// at-most-once claim is keyed on run/stage/PR/head, so a consumed claim meant
// Merge was NEVER re-attempted and the reconciliation row landing merged
// nothing, forever, with the reason discarded. The claim is now RELEASED on a
// Waiting return, which is sound because that return happens before any GitHub
// mutation.
func TestPipelineAutoMergeWaitingHoldClearsWhenTheConditionClears(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	const holdReason = "workload-mode change requires reconciliation at head 0123456 against operating-mode note 41"
	executor := &stubPipelineAutoMerger{
		readiness:   workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{Waiting: true, Reason: holdReason},
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)

	if len(executor.mergeReqs) != 1 {
		t.Fatalf("merge calls = %d, want one attempt", len(executor.mergeReqs))
	}
	if gate := stageRow(t, store, run.ID, "merge"); gate.State == StageBlocked || gate.State == StageFailed {
		t.Fatalf("a transient hold ended the run: gate=%+v run=%+v", gate, run)
	}
	// The hold is RECORDED, not silent: the pre-change terminal block carried the
	// reason and a bare wait threw it away.
	events, err := store.ListJobEvents(context.Background(), sourceJobID)
	if err != nil {
		t.Fatalf("ListJobEvents(source): %v", err)
	}
	held := ""
	for _, event := range events {
		switch event.Kind {
		case "pipeline_auto_merge_held":
			held = event.Message
		case "pipeline_auto_merge_confirmed":
			t.Fatalf("a held merge recorded a confirmation: %+v", event)
		}
	}
	if !strings.Contains(held, holdReason) {
		t.Fatalf("the hold reason was not recorded: %q", held)
	}

	// The condition clears. THIS is the property that matters: the next scan must
	// re-attempt and merge.
	executor.mergeResult = workflow.PipelineAutoMergeResult{Merged: true, MergeCommitSHA: "merge-after-hold"}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(3*time.Second), executor)
	if len(executor.mergeReqs) != 2 {
		t.Fatalf("merge calls = %d after the hold cleared, want a second attempt", len(executor.mergeReqs))
	}
	if run.State != RunSucceeded || stageRow(t, store, run.ID, "merge").State != StageSucceeded {
		t.Fatalf("run/gate = %+v / %+v, want succeeded once the hold cleared", run, stageRow(t, store, run.ID, "merge"))
	}

	// At-most-once still holds AFTER a real merge: the confirmed claim is not
	// released, so a later scan must not merge again.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(4*time.Second), executor)
	if len(executor.mergeReqs) != 2 {
		t.Fatalf("post-merge rescan merge calls = %d, want the claim to hold at two", len(executor.mergeReqs))
	}
}

// A hold that outlives the gate's timeout must park with its CAUSE, not only
// with what it was waiting for. The round-3 review measured an empty summary
// here, and a spec that sets no timeout waits forever - documented pipeline
// policy for gate stages, but only defensible while the hold is retryable and
// recorded, which the test above pins.
func TestPipelineAutoMergeHoldTimeoutCarriesTheReason(t *testing.T) {
	timedSpec := strings.Replace(pipelineAutoMergeSpec, "    merge: auto\n", "    merge: auto\n    timeout: 1m\n", 1)
	store := pipelineAdvanceStore(t)
	rec, spec := newTestPipeline(t, store, "auto-merge", timedSpec)
	enqueue := testStageEnqueuer(store)
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, spec, enqueue, now)
	impl := stageRow(t, store, run.ID, "impl")
	settleBoundImplementStageJob(t, store, impl.JobID, "implemented",
		PipelineStagePRBinding{PullRequest: 813, HeadSHA: "0123456789abcdef", Branch: "feat/813", TaskID: "task-813", LeadAgent: "coder"})
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(time.Second), &stubPipelineAutoMerger{})
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")

	const holdReason = "workload-mode change requires reconciliation at head 0123456 against operating-mode note 41"
	executor := &stubPipelineAutoMerger{
		readiness:   workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{Waiting: true, Reason: holdReason},
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Hour), executor)

	gate := stageRow(t, store, run.ID, "merge")
	if gate.State != StageBlocked {
		t.Fatalf("gate = %q after the timeout, want blocked", gate.State)
	}
	if !strings.Contains(gate.Summary, holdReason) {
		t.Fatalf("park summary must carry the cause: %q", gate.Summary)
	}
}

// With NO gate timeout the reviewer measured this parked silently at now+72h.
// The hold carries its own bound and parks WITH the cause.
//
// The MAGNITUDE is pinned with LITERALS. The first version derived its clock
// from autoMergeHoldMaxWait, so a mutant widening 6h to 720h - restoring exactly
// the unbounded wait this exists to remove - survived: a self-referential test
// cannot fail on the value it is meant to pin (#1783 round-4 review, F-5).
func TestPipelineAutoMergeHoldIsBoundedWithoutAGateTimeout(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	const holdReason = "workload-mode change requires reconciliation at head 0123456 against operating-mode note 41"
	executor := &stubPipelineAutoMerger{
		readiness:   workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{Waiting: true, Reason: holdReason},
	}
	for _, stage := range spec.Stages {
		if stage.ID == "merge" && strings.TrimSpace(stage.Timeout) != "" {
			t.Fatalf("fixture must have no gate timeout, got %q", stage.Timeout)
		}
	}

	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)
	if gate := stageRow(t, store, run.ID, "merge"); gate.State == StageBlocked {
		t.Fatalf("the first hold must wait, not park: %+v", gate)
	}
	held := newestRecordedHold(t, store, sourceJobID, "0123456789abcdef")
	if held.at.IsZero() {
		t.Fatalf("hold episode = %+v, want a recorded hold", held)
	}
	if held.reason != holdReason {
		t.Fatalf("episode reason = %q, want the hold's cause", held.reason)
	}

	// Just INSIDE six hours: still waiting, and still retrying.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, held.at.Add(24*time.Hour-time.Minute), executor)
	if len(executor.mergeReqs) != 2 {
		t.Fatalf("merge attempts inside the bound = %d, want 2", len(executor.mergeReqs))
	}
	if gate := stageRow(t, store, run.ID, "merge"); gate.State == StageBlocked {
		t.Fatalf("a hold 5h59m old must not park: %+v", gate)
	}

	// AT six hours exactly the bound applies: the comparison is >=, so the
	// boundary itself parks.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, held.at.Add(24*time.Hour), executor)
	gate := stageRow(t, store, run.ID, "merge")
	if gate.State != StageBlocked {
		t.Fatalf("gate = %q at exactly six hours, want blocked", gate.State)
	}
	for _, want := range []string{holdReason, "no gate timeout is set", "24h0m0s"} {
		if !strings.Contains(gate.Summary, want) {
			t.Fatalf("park summary must contain %q: %q", want, gate.Summary)
		}
	}
}

// The bound is per HOLD EPISODE, not per job. One row per job meant the anchor
// never reset: a brand-new hold hours later parked instantly and the recorded
// cause froze at the first reason (#1783 round-4 review, F-2).
func TestPipelineAutoMergeHoldBudgetResetsForANewEpisode(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	first := "workload-mode change requires reconciliation at head 0123456 against operating-mode note 41"
	executor := &stubPipelineAutoMerger{
		readiness:   workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{Waiting: true, Reason: first},
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)
	episode := newestRecordedHold(t, store, sourceJobID, "0123456789abcdef")
	if episode.at.IsZero() {
		t.Fatalf("first episode = %+v", episode)
	}

	// A day later a different DECISION holds. That is a new episode - a new
	// stable key, not merely new prose - and it must get its own budget rather
	// than parking instantly on the old anchor.
	second := "workload-mode change requires reconciliation at head 0123456 against operating-mode note 77"
	executor.mergeResult = workflow.PipelineAutoMergeResult{
		Waiting:           true,
		ReconciliationKey: "head=0123456789abcdef decision-note=77 mode=STEADY",
		Reason:            second,
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, episode.at.Add(25*time.Hour), executor)
	gate := stageRow(t, store, run.ID, "merge")
	if gate.State == StageBlocked {
		t.Fatalf("a new hold cause must start a new budget, not park instantly: %q", gate.Summary)
	}
	fresh := newestRecordedHold(t, store, sourceJobID, "0123456789abcdef")
	if fresh.reason != second {
		t.Fatalf("recorded cause = %q, want the CURRENT cause, not the frozen first one", fresh.reason)
	}
	if !fresh.at.After(episode.at) {
		t.Fatalf("new episode anchor %v must be later than the first %v", fresh.at, episode.at)
	}

	// And the new episode's own bound still applies a day after IT started.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, fresh.at.Add(24*time.Hour), executor)
	if gate := stageRow(t, store, run.ID, "merge"); gate.State != StageBlocked {
		t.Fatalf("gate = %q a day into the new episode, want blocked", gate.State)
	} else if !strings.Contains(gate.Summary, second) {
		t.Fatalf("park summary must carry the CURRENT cause: %q", gate.Summary)
	}
}

// THE ORDINARY PATH. ensureWorkloadModeReconciled runs in Evaluate BEFORE Merge
// with the same inputs, so a reconciliation hold normally surfaces as
// readiness.Waiting and never reaches the Merge-side instrumentation at all. The
// round-4 review measured that path recording no held event, carrying no reason
// and consulting no bound - running at now+72h with zero merge attempts, the
// round-3 defect verbatim on the common path (F-1).
func TestPipelineAutoMergeEvaluateSideHoldIsRecordedAndBounded(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	const holdReason = "workload-mode change requires reconciliation at head 0123456 against operating-mode note 41"
	executor := &stubPipelineAutoMerger{
		readiness: workflow.PipelineAutoMergeReadiness{
			Waiting: true, ReconciliationHold: true,
			CurrentHeadSHA: "0123456789abcdef", Reason: holdReason,
		},
	}

	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)
	if len(executor.mergeReqs) != 0 {
		t.Fatalf("a not-ready readiness must not attempt a merge: %d", len(executor.mergeReqs))
	}
	held := newestRecordedHold(t, store, sourceJobID, "0123456789abcdef")
	if held.at.IsZero() || held.reason != holdReason {
		t.Fatalf("the Evaluate-side hold was not recorded: %+v", held)
	}

	// Bounded on the same path: at six hours it parks WITH the cause.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, held.at.Add(24*time.Hour), executor)
	gate := stageRow(t, store, run.ID, "merge")
	if gate.State != StageBlocked {
		t.Fatalf("gate = %q six hours into an Evaluate-side hold, want blocked", gate.State)
	}
	if !strings.Contains(gate.Summary, holdReason) {
		t.Fatalf("park summary must carry the cause: %q", gate.Summary)
	}

	// A NON-reconciliation wait (CI pending) keeps the old cheap behaviour: no
	// hold row, no bound, so this fix cannot park a run waiting on checks.
	store2, enqueue2, rec2, spec2, run2, sourceJobID2, now2 := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store2, stageRow(t, store2, run2.ID, "review").JobID, "approved", "0123456789abcdef")
	ciWait := &stubPipelineAutoMerger{
		readiness: workflow.PipelineAutoMergeReadiness{
			Waiting: true, CurrentHeadSHA: "0123456789abcdef",
			Reason: "GitHub has not determined pull request mergeability yet",
		},
	}
	run2 = advanceWithAutoMerge(t, store2, enqueue2, rec2, spec2, run2, now2.Add(2*time.Second), ciWait)
	run2 = advanceWithAutoMerge(t, store2, enqueue2, rec2, spec2, run2, now2.Add(96*time.Hour), ciWait)
	if gate := stageRow(t, store2, run2.ID, "merge"); gate.State == StageBlocked {
		t.Fatalf("a CI wait must not be parked by the reconciliation bound: %q", gate.Summary)
	}
	if held := newestRecordedHold(t, store2, sourceJobID2, "0123456789abcdef"); !held.at.IsZero() {
		t.Fatalf("a CI wait must not record a reconciliation hold: %+v", held)
	}
}

// F-10, the round-5 blocking finding, entered through the production advancer.
// A hold record plus a lost claim used to be enough to KILL a run whose
// reconciliation had already succeeded: the !claimed branch consulted a record
// that is never cleared, and parked terminally on it. Reaching that branch means
// Evaluate reported Ready IN THIS PASS, so the record has already been
// contradicted by a live measurement.
func TestPipelineAutoMergeLostClaimNeverParksAResolvedHold(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	const holdReason = "workload-mode change requires reconciliation at head 0123456 against operating-mode note 41"
	executor := &stubPipelineAutoMerger{
		readiness:   workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{Waiting: true, Reason: holdReason},
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)
	held := newestRecordedHold(t, store, sourceJobID, "0123456789abcdef")
	if held.at.IsZero() {
		t.Fatalf("hold episode = %+v, want a recorded hold to go stale", held)
	}

	// The reconciliation LANDS: readiness is Ready and the merge would succeed.
	executor.mergeResult = workflow.PipelineAutoMergeResult{Merged: true, MergeCommitSHA: "merge-sha"}

	// And this scan loses the race - seeded exactly as a failed release leaves
	// it, by re-consuming the claim by hand. No age row is seeded because none
	// exists any more: the loser ages the wait from the gate stage's own
	// StartedAt, so there is no state in which the claim is consumed and the age
	// is missing (#1783 round-6 review, N-1).
	seedConsumedAutoMergeClaim(t, store, rec, run, sourceJobID)

	// Well past the hold's bound. The run must NOT be dead: the winner merges,
	// or the claim is released and the next scan merges.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, held.at.Add(autoMergeHoldMaxWait+time.Hour), executor)
	gate := stageRow(t, store, run.ID, "merge")
	if gate.State == StageBlocked {
		t.Fatalf("a lost claim must not park a run whose reconciliation resolved: %q", gate.Summary)
	}
	if strings.Contains(gate.Summary, holdReason) {
		t.Fatalf("summary must not blame a reconciliation that already succeeded: %q", gate.Summary)
	}
	if run.State == RunBlocked {
		t.Fatalf("run = %q, want a live run", run.State)
	}
}

// The other half of F-10: not parking must not mean going silent again, which
// was round-4's F-3. Past the bound the wait NAMES the orphaned claim and
// records it, and stays non-terminal.
func TestPipelineAutoMergeLostClaimReportsAnOrphanedClaim(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	executor := &stubPipelineAutoMerger{
		readiness:   workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{Merged: true, MergeCommitSHA: "merge-sha"},
	}
	// One advance promotes the queued gate row without claiming.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(time.Second),
		&stubPipelineAutoMerger{readiness: workflow.PipelineAutoMergeReadiness{Waiting: true, CurrentHeadSHA: "0123456789abcdef"}})
	seedConsumedAutoMergeClaim(t, store, rec, run, sourceJobID)
	// THE CLOCK HERE IS ANCHORED TO REAL TIME, deliberately. The bound compares
	// deps.now against the claim row's created_at, which the store writes from its
	// own CURRENT_TIMESTAMP, so a fixture clock set in the past yields a NEGATIVE
	// age and can never cross the bound. Round 6 rejected this whole design as
	// "unpinnable because a test cannot backdate created_at" - the obstacle was
	// the fixture's fixed 2026-07-11 clock, not the design (#1783 round-7).
	realNow := time.Now().UTC()

	// LITERALS, not autoMergeClaimOrphanAfter arithmetic. Deriving this clock
	// from the constant it pins made widening the bound to 720h invisible -
	// round-4's F-5, re-created here and caught by mutating my own guard.
	if autoMergeClaimOrphanAfter != 15*time.Minute {
		t.Fatalf("autoMergeClaimOrphanAfter = %s, want 15m0s; this test's clock is written in literals", autoMergeClaimOrphanAfter)
	}

	// Inside the window: waiting, and honest that another scan holds the claim.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, realNow.Add(14*time.Minute), executor)
	if gate := stageRow(t, store, run.ID, "merge"); gate.State == StageBlocked {
		t.Fatalf("a claim held inside the window must not park: %q", gate.Summary)
	}
	if events := autoMergeEventCount(t, store, sourceJobID, autoMergeClaimOrphanEventKind); events != 0 {
		t.Fatalf("suspicion rows inside the window = %d, want 0", events)
	}

	// Past it: still not parked, but the cause is recorded and stated.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, realNow.Add(16*time.Minute), executor)
	gate := stageRow(t, store, run.ID, "merge")
	if gate.State == StageBlocked {
		t.Fatalf("an orphaned claim is not a condition this scan observed, so it must not park: %q", gate.Summary)
	}
	if events := autoMergeEventCount(t, store, sourceJobID, autoMergeClaimOrphanEventKind); events != 1 {
		t.Fatalf("suspicion rows past the window = %d, want exactly 1", events)
	}
	// The EVENT is the durable record on this path, not the summary: a waiting
	// stage's summary is written only when it settles, so an operator with no
	// stage `timeout` has the event and nothing else. It has to name the claim.
	for _, want := range []string{`"head_sha":"0123456789abcdef"`, `"after":"15m0s"`, `"cause":"held_past_bound"`, `"claim_taken_at":"`} {
		if !strings.Contains(autoMergeEventBody(t, store, sourceJobID, autoMergeClaimOrphanEventKind), want) {
			t.Fatalf("suspicion row must contain %s: %s", want, autoMergeEventBody(t, store, sourceJobID, autoMergeClaimOrphanEventKind))
		}
	}
	// Recorded ONCE per claim, however many scans lose the race.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, realNow.Add(17*time.Minute), executor)
	if events := autoMergeEventCount(t, store, sourceJobID, autoMergeClaimOrphanEventKind); events != 1 {
		t.Fatalf("suspicion rows after a second losing scan = %d, want the same 1", events)
	}
}

// ROUND-8 F-1, and it is the reachable interleaving I asserted could not happen.
// A losing scan's failed ClaimJobEvent and its read are two un-transacted
// round-trips. Between them the winner can reach the Merge-side Waiting branch,
// record its hold and RELEASE the claim - the ordinary hold cycle, and now once
// per hold for every mode-marker PR because the mode gate runs at the merge
// boundary. The loser then finds no claim row, and collapsing that into
// "unreadable timestamp" wrote a durable, deduped, never-retracted orphan event
// on a healthy run.
//
// MY FIRST VERSION OF THIS TEST WAS VACUOUS and the mutants said so: it released
// the claim before advancing, so the next scan WON the claim and never entered
// the loser branch at all - it passed while testing nothing, and three mutants
// survived. The window only exists INSIDE a pass, so it is driven with the
// afterLostClaim seam, which deletes the row exactly where the winner's release
// would.
func TestPipelineAutoMergeLostClaimTreatsAReleasedClaimAsANormalRace(t *testing.T) {
	ctx := context.Background()
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(time.Second),
		&stubPipelineAutoMerger{readiness: workflow.PipelineAutoMergeReadiness{Waiting: true, CurrentHeadSHA: "0123456789abcdef"}})

	claim, err := json.Marshal(map[string]any{
		"phase": "claim", "pipeline": rec.Name, "run_id": run.ID,
		"stage_id": "merge", "pull_request": 813, "head_sha": "0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The claim is held, so this scan LOSES it...
	seedConsumedAutoMergeClaim(t, store, rec, run, sourceJobID)

	var gateStage Stage
	for _, candidate := range spec.Stages {
		if candidate.ID == "merge" {
			gateStage = candidate
			break
		}
	}
	released := false
	deps := pipelineStageSettleDeps{
		store: store, rec: rec, run: run,
		// Far past every bound, so a false report cannot hide behind a young claim.
		now: time.Now().UTC().Add(48 * time.Hour),
		autoMerge: &stubPipelineAutoMerger{
			readiness:   workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
			mergeResult: workflow.PipelineAutoMergeResult{Merged: true, MergeCommitSHA: "merge-sha"},
		},
		// ...and the winner releases it in the window before this scan reads it.
		afterLostClaim: func() {
			ok, relErr := store.ReleaseJobEventClaim(ctx, db.JobEvent{
				JobID: sourceJobID, Kind: autoMergeClaimEventKind, Message: string(claim),
			})
			if relErr != nil || !ok {
				t.Fatalf("releasing in the window: ok=%v err=%v", ok, relErr)
			}
			released = true
		},
	}
	settled, state, summary, _, _, err := gateStageSettleOutcome(ctx, deps, spec, gateStage, stageRow(t, store, run.ID, "merge"))
	if err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("the seam never fired, so this scan did not lose the claim and the test proves nothing")
	}
	if settled && state == StageBlocked {
		t.Fatalf("a released claim must not park: %q", summary)
	}
	if rows := autoMergeEventCount(t, store, sourceJobID, autoMergeClaimOrphanEventKind); rows != 0 {
		t.Fatalf("orphan reports for a RELEASED claim = %d, want 0 - a normal hold cycle must not look like a fault", rows)
	}
}

// F-6's calibration, pinned directly. The gate's StartedAt is stamped when the
// gate goes in-flight, typically long before readiness, so aging the wait from
// the GATE made a healthy two-second race on a gate that had waited past the
// bound write a durable "orphaned" event. Aging from the CLAIM makes a young
// claim young however old the gate is, and this drives exactly that shape: an
// hour-old gate, a claim taken seconds ago, no orphan report.
func TestPipelineAutoMergeLostClaimIgnoresGateAgeWhenTheClaimIsYoung(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(time.Second),
		&stubPipelineAutoMerger{readiness: workflow.PipelineAutoMergeReadiness{Waiting: true, CurrentHeadSHA: "0123456789abcdef"}})

	// The gate has been waiting for hours on the pipeline's clock.
	gate := stageRow(t, store, run.ID, "merge")
	realNow := time.Now().UTC()
	gate.StartedAt = realNow.Add(-6 * time.Hour)
	if err := store.UpdatePipelineRunStage(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	// The claim, by contrast, was taken just now.
	seedConsumedAutoMergeClaim(t, store, rec, run, sourceJobID)

	executor := &stubPipelineAutoMerger{
		readiness:   workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{Merged: true, MergeCommitSHA: "merge-sha"},
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, realNow.Add(time.Minute), executor)
	if gate := stageRow(t, store, run.ID, "merge"); gate.State == StageBlocked {
		t.Fatalf("a young claim must not park: %q", gate.Summary)
	}
	if rows := autoMergeEventCount(t, store, sourceJobID, autoMergeClaimOrphanEventKind); rows != 0 {
		t.Fatalf("orphan reports for a one-minute-old claim on a six-hour-old gate = %d, want 0", rows)
	}
}

// THE SUCCESS-PATH CONTROL for the claim clock, and it guards a real hazard.
// autoMergeClaimTakenAt reads the claim row's created_at and falls back to a
// "cannot age this" branch when it will not parse. If the store's timestamp
// format ever changed, that fallback would swallow EVERY claim and every losing
// scan would report an unreadable timestamp. This pins that the format the store
// actually writes does parse.
//
// The failure side is deliberately NOT tested here and that is stated rather
// than hidden: no store API can write created_at - every INSERT INTO job_events
// names only (job_id, kind, message), so the column is always its
// CURRENT_TIMESTAMP default. A malformed value comes from a corrupted or legacy
// row, which this package cannot construct. Round 6 manufactured an equivalent
// state by row surgery and the review called that out (#1783 round-7 F-3); the
// honest answer is a control on the reachable side plus this note, not a fixture
// that pins a state the system does not produce.
func TestPipelineAutoMergeClaimTimestampParsesTheStoreFormat(t *testing.T) {
	store, _, rec, _, run, sourceJobID, _ := prepareAutoMergeGate(t)
	claim, err := json.Marshal(map[string]any{
		"phase": "claim", "pipeline": rec.Name, "run_id": run.ID,
		"stage_id": "merge", "pull_request": 813, "head_sha": "0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC().Add(-2 * time.Minute)
	if claimed, err := store.ClaimJobEvent(context.Background(), db.JobEvent{
		JobID: sourceJobID, Kind: autoMergeClaimEventKind, Message: string(claim),
	}); err != nil || !claimed {
		t.Fatalf("seeding the claim: claimed=%v err=%v", claimed, err)
	}

	at, lookup, _, err := autoMergeClaimTakenAt(context.Background(), store, sourceJobID, string(claim))
	if err != nil {
		t.Fatal(err)
	}
	if lookup != autoMergeClaimFound {
		t.Fatal("the store's own created_at format must parse, or every losing scan reports an unreadable timestamp")
	}
	if at.Before(before) || at.After(time.Now().UTC().Add(2*time.Minute)) {
		t.Fatalf("claim taken at %v, want a value near now - the parse succeeded but the value is wrong", at)
	}
	// And a message that is not this claim must not be mistaken for it.
	if _, lookup, _, err := autoMergeClaimTakenAt(context.Background(), store, sourceJobID, `{"phase":"claim","other":true}`); err != nil {
		t.Fatal(err)
	} else if lookup != autoMergeClaimGone {
		t.Fatalf("a different claim message must report Gone, got %v", lookup)
	}
}

// F-11: the hold's budget is keyed on a STABLE discriminator, so churn in the
// volatile half of the cause cannot reset it. The near-miss text names whichever
// row the scan saw last, and one new reconciliation row anywhere in the repo
// changes it - which, keyed on the cause, opened a fresh episode with a full
// budget and deferred the bound indefinitely.
func TestPipelineAutoMergeHoldBudgetSurvivesVolatileCauseText(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	const stableKey = "head=0123456789abcdef decision-note=41 mode=STEADY"
	base := "workload-mode change requires reconciliation at head 0123456 against operating-mode note 41"
	executor := &stubPipelineAutoMerger{
		readiness: workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{
			Waiting: true, ReconciliationKey: stableKey, Reason: base,
		},
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)
	first := newestRecordedHold(t, store, sourceJobID, "0123456789abcdef")
	if first.at.IsZero() {
		t.Fatalf("first episode = %+v", first)
	}

	// The same hold, one scan later, with a DIFFERENT near-miss appended - the
	// exact churn F-11 describes.
	executor.mergeResult.Reason = base + "; row 99 reconciles head abcdef1, but the current head is 0123456"
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, first.at.Add(autoMergeHoldMaxWait-time.Minute), executor)
	if gate := stageRow(t, store, run.ID, "merge"); gate.State == StageBlocked {
		t.Fatalf("gate parked one minute early: %q", gate.Summary)
	}

	// Past the bound measured from the FIRST hold, the budget must be spent -
	// the churn must not have bought another full window.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, first.at.Add(autoMergeHoldMaxWait+time.Minute), executor)
	gate := stageRow(t, store, run.ID, "merge")
	if gate.State != StageBlocked {
		t.Fatalf("gate = %q past the bound, want blocked: churn in the cause must not reset the budget", gate.State)
	}
	// F-12: the park tells the operator what to DO about it, not only what fired.
	for _, want := range []string{"set a `timeout` on this gate stage", autoMergeHoldMaxWait.String()} {
		if !strings.Contains(gate.Summary, want) {
			t.Fatalf("park summary must contain %q: %q", want, gate.Summary)
		}
	}
}

// seedConsumedAutoMergeClaim leaves the at-most-once claim consumed, exactly as
// a winner that died mid-write or a failed release DELETE leaves it.
func seedConsumedAutoMergeClaim(t *testing.T, store *db.Store, rec db.Pipeline, run db.PipelineRun, sourceJobID string) {
	t.Helper()
	claim, err := json.Marshal(map[string]any{
		"phase": "claim", "pipeline": rec.Name, "run_id": run.ID,
		"stage_id": "merge", "pull_request": 813, "head_sha": "0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimJobEvent(context.Background(), db.JobEvent{
		JobID: sourceJobID, Kind: autoMergeClaimEventKind, Message: string(claim),
	}); err != nil || !claimed {
		t.Fatalf("seeding the consumed claim: claimed=%v err=%v", claimed, err)
	}
}

func autoMergeEventBody(t *testing.T, store *db.Store, sourceJobID, kind string) string {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), sourceJobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	for _, event := range events {
		if event.Kind == kind {
			return event.Message
		}
	}
	return ""
}

func autoMergeEventCount(t *testing.T, store *db.Store, sourceJobID, kind string) int {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), sourceJobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

// Two scans racing the same NEW episode each write a held row, which is
// deliberate and harmless only because the anchor takes the EARLIEST held_at:
// anchoring on the latest would restart the budget on every duplicate and the
// bound would never be reached (#1783 round-4 review, F-5 listed this guard as
// unkillable - it is killable once a duplicate exists, which is the state the
// check-then-insert race actually produces).
func TestPipelineAutoMergeHoldAnchorsOnTheEarliestDuplicate(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	const holdReason = "workload-mode change requires reconciliation at head 0123456 against operating-mode note 41"
	executor := &stubPipelineAutoMerger{
		readiness: workflow.PipelineAutoMergeReadiness{
			Waiting: true, ReconciliationHold: true,
			CurrentHeadSHA: "0123456789abcdef", Reason: holdReason,
		},
	}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(2*time.Second), executor)
	first := newestRecordedHold(t, store, sourceJobID, "0123456789abcdef")
	if first.at.IsZero() {
		t.Fatalf("first episode = %+v", first)
	}

	// The racing scan's row: same head and cause, a LATER held_at.
	duplicate, marshalErr := json.Marshal(map[string]any{
		"phase": "held", "pipeline": rec.Name, "run_id": run.ID,
		"stage_id": "merge", "pull_request": 813, "head_sha": "0123456789abcdef",
		"reason": holdReason, "episode_key": "head=0123456789abcdef",
		"held_at": first.at.Add(23 * time.Hour).Format(time.RFC3339Nano),
	})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err := store.AddJobEvent(context.Background(), db.JobEvent{
		JobID: sourceJobID, Kind: "pipeline_auto_merge_held", Message: string(duplicate),
	}); err != nil {
		t.Fatal(err)
	}

	// Asserted through the EPISODE resolver, which is what production reads.
	// newestRecordedHold deliberately reports the newest row, so it would report
	// the duplicate and prove nothing about the anchor.
	anchored, err := autoMergeHoldEpisode(context.Background(), store, sourceJobID, "0123456789abcdef", "head=0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if !anchored.at.Equal(first.at) {
		t.Fatalf("anchor = %v, want the EARLIEST duplicate %v", anchored.at, first.at)
	}

	// And the bound still fires a day after the TRUE start, not after the
	// duplicate: anchoring late would leave this waiting.
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, first.at.Add(24*time.Hour), executor)
	if gate := stageRow(t, store, run.ID, "merge"); gate.State != StageBlocked {
		t.Fatalf("gate = %q a day after the episode began, want blocked", gate.State)
	}
}

// F-3's ordering, pinned by INJECTING the failure it exists for. A lost release
// must leave the hold RECORDED, because the !claimed branch bounds the run from
// that record; recording after the release left one transient DELETE error with
// a consumed claim and no record, which is the unbounded silent wait the review
// measured at now+72h. Nothing else distinguishes the two orderings, so without
// this the correct order survives its own mutant.
func TestPipelineAutoMergeHoldIsRecordedEvenWhenTheReleaseFails(t *testing.T) {
	store, enqueue, rec, spec, run, sourceJobID, now := prepareAutoMergeGate(t)
	settleBoundReviewJob(t, store, stageRow(t, store, run.ID, "review").JobID, "approved", "0123456789abcdef")
	const holdReason = "workload-mode change requires reconciliation at head 0123456 against operating-mode note 41"
	// One advance moves the gate row from queued to running; the settle path
	// promotes a queued row and returns before it can claim.
	waiting := &stubPipelineAutoMerger{readiness: workflow.PipelineAutoMergeReadiness{Waiting: true, CurrentHeadSHA: "0123456789abcdef"}}
	run = advanceWithAutoMerge(t, store, enqueue, rec, spec, run, now.Add(time.Second), waiting)

	executor := &stubPipelineAutoMerger{
		readiness:   workflow.PipelineAutoMergeReadiness{Ready: true, CurrentHeadSHA: "0123456789abcdef"},
		mergeResult: workflow.PipelineAutoMergeResult{Waiting: true, Reason: holdReason},
	}
	var gateStage Stage
	for _, candidate := range spec.Stages {
		if candidate.ID == "merge" {
			gateStage = candidate
		}
	}
	deps := pipelineStageSettleDeps{
		store: store, rec: rec, run: run, now: now.Add(2 * time.Second), autoMerge: executor,
		releaseClaim: func(context.Context, db.JobEvent) (bool, error) {
			return false, errors.New("injected release failure")
		},
	}

	_, _, _, _, _, err := gateStageSettleOutcome(context.Background(), deps, spec, gateStage, stageRow(t, store, run.ID, "merge"))
	if err == nil {
		t.Fatal("a failing release must surface its error")
	}
	if len(executor.mergeReqs) != 1 {
		t.Fatalf("merge attempts = %d, want the hold path to have run", len(executor.mergeReqs))
	}

	// The hold survived the failed release, so the next scan is bounded rather
	// than silent - which is the whole point of the ordering.
	held := newestRecordedHold(t, store, sourceJobID, "0123456789abcdef")
	if held.at.IsZero() || held.reason != holdReason {
		t.Fatalf("hold record after a failed release = %+v, want the cause recorded", held)
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

// newestRecordedHold is the TESTS' inspection reader: the newest hold row for a
// head, whatever its episode key. Production has no such reader by design - a
// scan reads a hold only for a cause it observed in the same pass - and this
// parses the row independently rather than reusing autoMergeHoldEpisode, so a
// bug in that lookup cannot hide behind a test that shares it.
func newestRecordedHold(t *testing.T, store *db.Store, sourceJobID, head string) autoMergeHold {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), sourceJobID)
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	var newest autoMergeHold
	for _, event := range events {
		if event.Kind != autoMergeHeldEventKind {
			continue
		}
		var body struct {
			HeadSHA string `json:"head_sha"`
			Reason  string `json:"reason"`
			HeldAt  string `json:"held_at"`
		}
		if json.Unmarshal([]byte(event.Message), &body) != nil {
			continue
		}
		if strings.TrimSpace(body.HeadSHA) != head {
			continue
		}
		at, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(body.HeldAt))
		if parseErr != nil {
			continue
		}
		if newest.at.IsZero() || at.After(newest.at) {
			newest = autoMergeHold{at: at.UTC(), reason: strings.TrimSpace(body.Reason)}
		}
	}
	return newest
}
