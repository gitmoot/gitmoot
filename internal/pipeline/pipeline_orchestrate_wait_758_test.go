package pipeline

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #758 WAIT: an orchestrate stage is a bounded agent SUB-TREE root. The coordinator
// settles the INSTANT it returns delegations, so the stage row cannot fold on that
// job — it must FOLLOW the deterministic <jobID>/continuation chain to the terminal
// tail and fold the TAIL's decision. These tests drive the settle seam over seeded
// rows (no LLM), proving: the walk re-points JobID one hop per scan; it folds the
// TAIL (not the coordinator's approved decision); a simulated daemon restart re-derives
// the wait from rows without re-enqueuing a duplicate tree; and a stage-timeout trips
// the kill switch which routes the chain to a foldable finalize tail.

const orchestrateWaitSpec = `name: orch
repo: owner/repo
stages:
  - id: coord
    agent: planner
    prompt: decompose and synthesize
    orchestrate: true
`

const orchestrateWaitTimeoutSpec = `name: orch
repo: owner/repo
stages:
  - id: coord
    agent: planner
    prompt: decompose and synthesize
    orchestrate: true
    timeout: 1h
`

// seedContinuationJob mints a chain continuation job row (queued) at the deterministic
// id, carrying the sub-tree RootJobID so the seam's root resolution and re-point walk
// see exactly what the engine would have persisted.
func seedContinuationJob(t *testing.T, store *db.Store, id, parentID, rootID string) {
	t.Helper()
	mailbox := workflow.Mailbox{Store: store}
	if _, err := mailbox.Enqueue(context.Background(), workflow.JobRequest{
		ID:          id,
		Agent:       "planner",
		Action:      "ask",
		Repo:        "owner/repo",
		Sender:      "planner",
		ParentJobID: parentID,
		RootJobID:   rootID,
	}); err != nil {
		t.Fatalf("seed continuation %s: %v", id, err)
	}
}

// settleChainJob transitions an existing (queued) chain job to the terminal state its
// decision implies, attaching a Result with the given delegations and, optionally, the
// DelegationFinalize flag — so the seam sees a coordinator that fanned out (delegations)
// or a foldable tail (zero delegations, or a finalize job whose ignored delegations
// must NOT keep the stage waiting).
func settleChainJob(t *testing.T, store *db.Store, jobID, decision, summary string, needs []string, dels []workflow.Delegation, finalize bool) {
	t.Helper()
	ctx := context.Background()
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob(%s): %v", jobID, err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(%s): %v", jobID, err)
	}
	payload.Result = &workflow.AgentResult{Decision: decision, Summary: summary, Needs: needs, Delegations: dels}
	payload.DelegationFinalize = finalize
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	to := jobStateForDecision(decision)
	ok, err := store.TransitionJobStatePayloadWithEvent(ctx, jobID, job.State, to, string(encoded),
		db.JobEvent{JobID: jobID, Kind: to, Message: "settled by test"})
	if err != nil || !ok {
		t.Fatalf("settle %s -> %s: ok=%v err=%v", jobID, to, ok, err)
	}
}

func oneDelegation() []workflow.Delegation {
	return []workflow.Delegation{{ID: "leg", Agent: "worker", Action: "review", Prompt: "inspect"}}
}

// TestOrchestrateStageFollowsContinuationChainAndFoldsTail is the full WAIT E2E: a
// coordinator fans out (approved + delegations), a first continuation generation fans
// out AGAIN, and only the second continuation is a zero-delegation synthesis TAIL. The
// stage row must re-point its JobID one hop per scan across both generations and then
// fold the TAIL's decision — never the coordinator's approved.
func TestOrchestrateStageFollowsContinuationChainAndFoldsTail(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "orch", orchestrateWaitSpec)
	now := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	coordJob := stageRow(t, store, run.ID, "coord").JobID
	if coordJob == "" {
		t.Fatal("orchestrate coordinator stage was not enqueued")
	}
	// Guard the premise: the stage is classified orchestrate and the coordinator job is
	// its OWN sub-tree root (RootJobID = its own id, the one #757 deviation).
	if k := spec.Stages[0].Kind(); k != StageKindOrchestrate {
		t.Fatalf("stage kind = %v, want orchestrate", k)
	}
	if cj, _ := store.GetJob(context.Background(), coordJob); mustPayloadRoot(t, cj) != coordJob {
		t.Fatalf("coordinator RootJobID = %q, want its own id %q", mustPayloadRoot(t, cj), coordJob)
	}
	rootStartedAt := now.Add(10 * time.Second)
	startStageJob(t, store, coordJob)
	setPipelineJobEventTime(t, store, coordJob, string(workflow.JobRunning), rootStartedAt)
	run = advance(t, store, rec, spec, enqueue, run, rootStartedAt.Add(time.Second))
	if got := stageRow(t, store, run.ID, "coord"); !got.StartedAt.Equal(rootStartedAt) {
		t.Fatalf("orchestrate root start = %s, want running event %s", got.StartedAt, rootStartedAt)
	}

	contID := coordJob + "/continuation"
	contID2 := contID + "/continuation"
	// Seed the whole chain settled: coordinator + gen-1 both fan out; gen-2 is the tail.
	settleChainJob(t, store, coordJob, "approved", "coordinator placeholder", nil, oneDelegation(), false)
	seedContinuationJob(t, store, contID, coordJob, coordJob)
	settleChainJob(t, store, contID, "approved", "gen-1 placeholder", nil, oneDelegation(), false)
	seedContinuationJob(t, store, contID2, contID, coordJob)
	settleChainJob(t, store, contID2, "approved", "final synthesis", nil, nil, false)
	tailFinishedAt := now.Add(2 * time.Minute)
	setPipelineJobEventTime(t, store, contID2, string(workflow.JobSucceeded), tailFinishedAt)

	// Scan 1: coordinator settled + continuation exists → re-point to gen-1, stay running.
	run = advance(t, store, rec, spec, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "coord"); got.JobID != contID || got.State != StageRunning {
		t.Fatalf("after scan 1: stage = {JobID=%q,State=%s}, want re-pointed to %q running", got.JobID, got.State, contID)
	}
	if run.State != RunRunning {
		t.Fatalf("after scan 1: run = %s, want running", run.State)
	}

	// Scan 2: gen-1 settled + its continuation exists → re-point to gen-2 (the tail).
	run = advance(t, store, rec, spec, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "coord"); got.JobID != contID2 {
		t.Fatalf("after scan 2: stage JobID = %q, want re-pointed to the tail %q", got.JobID, contID2)
	}
	if run.State != RunRunning {
		t.Fatalf("after scan 2: run = %s, want running", run.State)
	}

	// Scan 3: the tail is settled with no continuation and zero delegations → FOLD it.
	run = advance(t, store, rec, spec, enqueue, run, now)
	got := stageRow(t, store, run.ID, "coord")
	if got.State != StageSucceeded {
		t.Fatalf("after scan 3: stage = %s, want succeeded (fold the tail)", got.State)
	}
	if got.Summary != "final synthesis" {
		t.Fatalf("stage folded summary = %q, want the TAIL's %q (not the coordinator's)", got.Summary, "final synthesis")
	}
	if !got.StartedAt.Equal(rootStartedAt) || !got.FinishedAt.Equal(tailFinishedAt) {
		t.Fatalf("orchestrate times = (%s, %s), want root start %s and tail finish %s", got.StartedAt, got.FinishedAt, rootStartedAt, tailFinishedAt)
	}
	if run.State != RunSucceeded {
		t.Fatalf("run = %s, want succeeded", run.State)
	}
}

// TestOrchestrateStageFoldsTailBlockedDecision decisively proves the stage folds the
// TAIL, not the coordinator: the coordinator returns approved, but the synthesis tail
// returns blocked with needs. The stage must fold BLOCKED and the run park blocked with
// the tail's needs — the coordinator's approved must never settle the stage.
func TestOrchestrateStageFoldsTailBlockedDecision(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "orch", orchestrateWaitSpec)
	now := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	coordJob := stageRow(t, store, run.ID, "coord").JobID
	contID := coordJob + "/continuation"

	settleChainJob(t, store, coordJob, "approved", "coordinator approved", nil, oneDelegation(), false)
	seedContinuationJob(t, store, contID, coordJob, coordJob)
	settleChainJob(t, store, contID, "blocked", "the sub-tree needs a secret", []string{"R2 token"}, nil, false)

	run = advance(t, store, rec, spec, enqueue, run, now) // re-point to the tail
	run = advance(t, store, rec, spec, enqueue, run, now) // fold the tail

	if got := stageRow(t, store, run.ID, "coord"); got.State != StageBlocked {
		t.Fatalf("stage = %s, want blocked (fold the tail, not the coordinator's approved)", got.State)
	}
	if run.State != RunBlocked {
		t.Fatalf("run = %s, want blocked", run.State)
	}
	if got := decodePipelineNeeds(run.NeedsJSON); len(got) != 1 || got[0] != "R2 token" {
		t.Fatalf("run needs = %v, want the tail's [R2 token]", got)
	}
}

// TestOrchestrateStageMidFlightCoordinatorDoesNotFold pins the critical guard: a
// coordinator that settled WITH live delegations but whose continuation is not yet
// minted (children still running) must NOT fold — its approved decision is not the
// stage outcome. The stage stays in flight until the continuation row appears.
func TestOrchestrateStageMidFlightCoordinatorDoesNotFold(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "orch", orchestrateWaitSpec)
	now := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	coordJob := stageRow(t, store, run.ID, "coord").JobID
	// Coordinator settled approved WITH delegations; no continuation seeded yet.
	settleChainJob(t, store, coordJob, "approved", "fanned out", nil, oneDelegation(), false)

	run = advance(t, store, rec, spec, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "coord"); got.State == StageSucceeded || got.State == StageFailed {
		t.Fatalf("stage = %s, want still in flight — a mid-flight coordinator must not fold", got.State)
	}
	if got := stageRow(t, store, run.ID, "coord"); got.JobID != coordJob {
		t.Fatalf("stage JobID = %q, want unchanged %q (no continuation to walk to yet)", got.JobID, coordJob)
	}
	if run.State != RunRunning {
		t.Fatalf("run = %s, want running while the sub-tree is live", run.State)
	}
}

// TestOrchestrateStageRestartReconnectsWithoutDuplicateTree proves the restart
// property: the wait is re-derived purely from rows, so a fresh advance over the same
// rows (a simulated daemon restart) reconnects to the in-flight sub-tree — it re-points
// to the live continuation and does NOT enqueue a duplicate tree.
func TestOrchestrateStageRestartReconnectsWithoutDuplicateTree(t *testing.T) {
	store := pipelineAdvanceStore(t)
	ctx := context.Background()
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "orch", orchestrateWaitSpec)
	now := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	coordJob := stageRow(t, store, run.ID, "coord").JobID
	contID := coordJob + "/continuation"

	// Mid-flight: coordinator fanned out, continuation minted but still running.
	settleChainJob(t, store, coordJob, "approved", "coordinator", nil, oneDelegation(), false)
	seedContinuationJob(t, store, contID, coordJob, coordJob)

	// One scan re-points the stage to the live (unsettled) continuation.
	run = advance(t, store, rec, spec, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "coord"); got.JobID != contID {
		t.Fatalf("stage JobID = %q, want re-pointed to the live continuation %q", got.JobID, contID)
	}
	subTreeBefore := countSubTreeJobs(t, store, coordJob)

	// Simulate a daemon restart: reload the run from the store (no in-memory wait state
	// survives) and advance again. The wait reconnects from rows.
	reloaded, ok, err := store.GetPipelineRun(ctx, run.ID)
	if err != nil || !ok {
		t.Fatalf("GetPipelineRun: ok=%v err=%v", ok, err)
	}
	reloaded = advance(t, store, rec, spec, enqueue, reloaded, now)

	// No duplicate tree: the enqueue pass must NOT mint a fresh orchestrate coordinator
	// (the stage is in-flight, not pending), so the sub-tree job set is unchanged.
	if after := countSubTreeJobs(t, store, coordJob); after != subTreeBefore {
		t.Fatalf("restart enqueued a duplicate tree: sub-tree jobs before=%d after=%d", subTreeBefore, after)
	}
	if got := stageRow(t, store, reloaded.ID, "coord"); got.JobID != contID {
		t.Fatalf("after restart: stage JobID = %q, want still reconnected to %q", got.JobID, contID)
	}
	if got := stageRow(t, store, reloaded.ID, "coord"); got.Attempt != 0 {
		t.Fatalf("after restart: stage attempt = %d, want 0 (no fresh tree)", got.Attempt)
	}
	if reloaded.State != RunRunning {
		t.Fatalf("after restart: run = %s, want still running", reloaded.State)
	}

	// The tail finally arrives: the reconnected walk folds it normally.
	settleChainJob(t, store, contID, "approved", "final", nil, nil, false)
	reloaded = advance(t, store, rec, spec, enqueue, reloaded, now)
	if got := stageRow(t, store, reloaded.ID, "coord"); got.State != StageSucceeded {
		t.Fatalf("after tail: stage = %s, want succeeded", got.State)
	}
	if reloaded.State != RunSucceeded {
		t.Fatalf("after tail: run = %s, want succeeded", reloaded.State)
	}
}

// TestOrchestrateStageTimeoutTripsKillAndFoldsFinalizeTail proves the timeout bound:
// once the stage exceeds its timeout the scan trips the #341 kill switch on the sub-tree
// root (once, idempotently); the engine's kill-routed FINALIZE continuation (seeded here)
// is then walked and folded as the tail — even though the finalize job's stored Result
// still carries ignored delegations. So a timed-out sub-tree always ends in a foldable tail.
func TestOrchestrateStageTimeoutTripsKillAndFoldsFinalizeTail(t *testing.T) {
	store := pipelineAdvanceStore(t)
	ctx := context.Background()
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "orch", orchestrateWaitTimeoutSpec)
	start := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, start)
	coordJob := stageRow(t, store, run.ID, "coord").JobID
	contID := coordJob + "/continuation"
	realStart := start.Add(30 * time.Minute)
	reclaimedAt := realStart.Add(15 * time.Minute)
	startStageJob(t, store, coordJob)
	ok, err := store.TransitionJobStateWithEvent(ctx, coordJob, string(workflow.JobRunning), string(workflow.JobQueued), db.JobEvent{
		JobID: coordJob, Kind: string(workflow.JobQueued), Message: "requeued after reboot by test",
	})
	if err != nil || !ok {
		t.Fatalf("requeue coordinator: ok=%v err=%v", ok, err)
	}
	startStageJob(t, store, coordJob)
	setPipelineJobEventTimeAtIndex(t, store, coordJob, string(workflow.JobRunning), 0, realStart)
	setPipelineJobEventTimeAtIndex(t, store, coordJob, string(workflow.JobRunning), 1, reclaimedAt)
	run = advance(t, store, rec, spec, enqueue, run, reclaimedAt.Add(time.Second))
	if got := stageRow(t, store, run.ID, "coord"); !got.StartedAt.Equal(realStart) {
		t.Fatalf("orchestrate timeout anchor after reclaim = %s, want earliest running-event start %s", got.StartedAt, realStart)
	}

	// Coordinator fanned out; its sub-tree is still live (no continuation yet).
	settleChainJob(t, store, coordJob, "approved", "coordinator", nil, oneDelegation(), false)

	// Queue wait is excluded: only 45 minutes have elapsed from the first running
	// event, so the timeout must not trip yet.
	beforeRealTimeout := start.Add(75 * time.Minute)
	run = advance(t, store, rec, spec, enqueue, run, beforeRealTimeout)
	if killed, err := store.IsRootJobKilled(ctx, coordJob); err != nil || killed {
		t.Fatalf("timeout measured from enqueue instead of real start: killed=%v err=%v", killed, err)
	}

	// A scan past one hour from the FIRST start trips the kill switch even though the
	// reclaim was only 45 minutes ago. The timeout is reboot-stable.
	past := realStart.Add(time.Hour + time.Second)
	run = advance(t, store, rec, spec, enqueue, run, past)
	if killed, err := store.IsRootJobKilled(ctx, coordJob); err != nil || !killed {
		t.Fatalf("stage timeout must trip the #341 kill switch on the sub-tree root: killed=%v err=%v", killed, err)
	}
	if run.State != RunRunning {
		t.Fatalf("run = %s, want still running (awaiting the finalize tail)", run.State)
	}
	killEventsAfterTrip := countJobEvents(t, store, coordJob, "delegation_killed")
	if killEventsAfterTrip != 1 {
		t.Fatalf("delegation_killed events = %d, want exactly 1", killEventsAfterTrip)
	}

	// A second past-timeout scan must NOT re-kill (idempotent trip).
	run = advance(t, store, rec, spec, enqueue, run, past)
	if got := countJobEvents(t, store, coordJob, "delegation_killed"); got != 1 {
		t.Fatalf("delegation_killed events after re-scan = %d, want 1 (idempotent)", got)
	}

	// The engine's kill routes the frontier to a FINALIZE continuation (DelegationFinalize),
	// which may still carry ignored delegations. Seed it and prove the walk folds it as the tail.
	seedContinuationJob(t, store, contID, coordJob, coordJob)
	settleChainJob(t, store, contID, "approved", "finalized after timeout", nil, oneDelegation(), true)

	run = advance(t, store, rec, spec, enqueue, run, past) // re-point to the finalize continuation
	run = advance(t, store, rec, spec, enqueue, run, past) // fold the finalize tail
	got := stageRow(t, store, run.ID, "coord")
	if got.State != StageSucceeded {
		t.Fatalf("stage = %s, want succeeded (fold the finalize tail)", got.State)
	}
	if got.Summary != "finalized after timeout" {
		t.Fatalf("stage summary = %q, want the finalize tail's %q", got.Summary, "finalized after timeout")
	}
	if run.State != RunSucceeded {
		t.Fatalf("run = %s, want succeeded", run.State)
	}
}

// mustPayloadRoot returns a job's payload RootJobID (test helper).
func mustPayloadRoot(t *testing.T, job db.Job) string {
	t.Helper()
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload: %v", err)
	}
	return payload.RootJobID
}

// countSubTreeJobs counts jobs belonging to the orchestrate sub-tree (RootJobID = the
// coordinator's own id). Unlike shell/#757 stages, an orchestrate stage job is its OWN
// root, so its tree is NOT under run.ID — count by the coordinator id.
func countSubTreeJobs(t *testing.T, store *db.Store, coordJobID string) int {
	t.Helper()
	jobs, err := store.ListJobsByRoot(context.Background(), coordJobID)
	if err != nil {
		t.Fatalf("ListJobsByRoot(%s): %v", coordJobID, err)
	}
	return len(jobs)
}

// countJobEvents counts job events of a kind (test helper local to the CLI package).
func countJobEvents(t *testing.T, store *db.Store, jobID, kind string) int {
	t.Helper()
	events, err := store.ListJobEvents(context.Background(), jobID)
	if err != nil {
		t.Fatalf("ListJobEvents(%s): %v", jobID, err)
	}
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}
