package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// pipelineAdvanceStore opens an isolated store for advancer unit tests. The
// advancer reads/writes real rows (run/stage/job), so a real store — not a mock —
// is the honest test double; it never touches a user home.
func pipelineAdvanceStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// newTestPipeline stores a pipeline record built from spec YAML and returns the
// canonical stored record plus the parsed spec.
func newTestPipeline(t *testing.T, store *db.Store, name, specYAML string) (db.Pipeline, Spec) {
	t.Helper()
	spec, err := Load([]byte(specYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := db.Pipeline{Name: name, Repo: "owner/repo", SpecYAML: specYAML, SpecHash: Hash([]byte(specYAML))}
	if err := store.CreateOrUpdatePipeline(context.Background(), rec); err != nil {
		t.Fatalf("CreateOrUpdatePipeline: %v", err)
	}
	got, ok, err := store.GetPipeline(context.Background(), name)
	if err != nil || !ok {
		t.Fatalf("GetPipeline: ok=%v err=%v", ok, err)
	}
	return got, spec
}

// testStageEnqueuer is a real Mailbox-backed enqueuer: it persists a queued job
// row so the advancer's later GetJob(stage.JobID) fold works. It requires no
// agent (Mailbox tolerates a missing agent) — the unit tests exercise the
// advancer's decision logic, not the worker.
func testStageEnqueuer(store *db.Store) PipelineStageEnqueuer {
	mailbox := workflow.Mailbox{Store: store}
	return func(ctx context.Context, request workflow.JobRequest) (db.Job, error) {
		return mailbox.Enqueue(ctx, request)
	}
}

// settleStageJob transitions a stage's enqueued job to the terminal state the real
// mailbox would produce for a given decision (stateForDecision), attaching the
// result. It faithfully reproduces the changes_requested TRAP: a changes_requested
// job settles to jobs.state = SUCCEEDED, so an advancer that folded on jobs.state
// would wrongly advance it.
func settleStageJob(t *testing.T, store *db.Store, jobID, decision, summary string, needs []string) {
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
	payload.Result = &workflow.AgentResult{Decision: decision, Summary: summary, Needs: needs}
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

func startStageJob(t *testing.T, store *db.Store, jobID string) {
	t.Helper()
	ok, err := store.TransitionJobStateWithEvent(context.Background(), jobID, string(workflow.JobQueued), string(workflow.JobRunning), db.JobEvent{
		JobID: jobID, Kind: string(workflow.JobRunning), Message: "job started by test",
	})
	if err != nil || !ok {
		t.Fatalf("start %s: ok=%v err=%v", jobID, ok, err)
	}
}

func setPipelineJobEventTime(t *testing.T, store *db.Store, jobID, kind string, at time.Time) {
	t.Helper()
	conn, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	result, err := conn.Exec(`UPDATE job_events SET created_at = ? WHERE job_id = ? AND kind = ?`, at.UTC().Format(time.RFC3339Nano), jobID, kind)
	if err != nil {
		t.Fatalf("UPDATE job event time: %v", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		t.Fatalf("updated job event rows = %d, err=%v, want 1", changed, err)
	}
}

func setPipelineJobEventTimeAtIndex(t *testing.T, store *db.Store, jobID, kind string, index int, at time.Time) {
	t.Helper()
	conn, err := sql.Open("sqlite", store.DatabasePath())
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	result, err := conn.Exec(`UPDATE job_events SET created_at = ? WHERE id = (
		SELECT id FROM job_events WHERE job_id = ? AND kind = ? ORDER BY id ASC LIMIT 1 OFFSET ?
	)`, at.UTC().Format(time.RFC3339Nano), jobID, kind, index)
	if err != nil {
		t.Fatalf("UPDATE indexed job event time: %v", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		t.Fatalf("updated indexed job event rows = %d, err=%v, want 1", changed, err)
	}
}

// jobStateForDecision mirrors workflow.stateForDecision (unexported): blocked and
// failed are their own terminal states; everything else (approved, implemented,
// AND changes_requested) is a SUCCEEDED job.
func jobStateForDecision(decision string) string {
	switch decision {
	case "blocked":
		return string(workflow.JobBlocked)
	case "failed":
		return string(workflow.JobFailed)
	default:
		return string(workflow.JobSucceeded)
	}
}

func stageRow(t *testing.T, store *db.Store, runID, stageID string) db.PipelineRunStage {
	t.Helper()
	stage, ok, err := store.GetPipelineRunStage(context.Background(), runID, stageID)
	if err != nil || !ok {
		t.Fatalf("GetPipelineRunStage(%s/%s): ok=%v err=%v", runID, stageID, ok, err)
	}
	return stage
}

func startTestRun(t *testing.T, store *db.Store, rec db.Pipeline, spec Spec, enqueue PipelineStageEnqueuer, now time.Time) db.PipelineRun {
	t.Helper()
	run, err := CreatePipelineRun(context.Background(), store, rec, spec, "manual", "{}", now)
	if err != nil {
		t.Fatalf("CreatePipelineRun: %v", err)
	}
	run, err = AdvancePipelineRun(context.Background(), store, enqueue, rec, spec, run, now)
	if err != nil {
		t.Fatalf("initial advance: %v", err)
	}
	return run
}

func advance(t *testing.T, store *db.Store, rec db.Pipeline, spec Spec, enqueue PipelineStageEnqueuer, run db.PipelineRun, now time.Time) db.PipelineRun {
	t.Helper()
	updated, err := AdvancePipelineRun(context.Background(), store, enqueue, rec, spec, run, now)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	return updated
}

const linearChainSpec = `name: chain
repo: owner/repo
stages:
  - id: a
    cmd: echo a
  - id: b
    cmd: echo b
    needs: [a]
  - id: c
    cmd: echo c
    needs: [b]
`

func TestCreatePipelineRunPersistsStageDeps(t *testing.T) {
	store := pipelineAdvanceStore(t)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	now := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)

	run, err := CreatePipelineRun(context.Background(), store, rec, spec, "manual", "{}", now)
	if err != nil {
		t.Fatalf("CreatePipelineRun: %v", err)
	}
	for _, tc := range []struct {
		stageID string
		want    string
	}{
		{stageID: "a", want: ""},
		{stageID: "b", want: `["a"]`},
		{stageID: "c", want: `["b"]`},
	} {
		if got := stageRow(t, store, run.ID, tc.stageID).NeedsJSON; got != tc.want {
			t.Fatalf("stage %s needs_json = %q, want %q", tc.stageID, got, tc.want)
		}
	}
}

// TestAdvancerLinearChain proves a linear a->b->c chain advances one layer per
// scan: each stage is enqueued only after its single dependency succeeds, and the
// run reaches succeeded exactly when the last stage does.
func TestAdvancerLinearChain(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	// Only the root is enqueued; b/c wait on their deps.
	if got := stageRow(t, store, run.ID, "a"); got.State != StageQueued || got.JobID == "" {
		t.Fatalf("stage a = %+v, want queued with job", got)
	}
	if got := stageRow(t, store, run.ID, "b"); got.State != StagePending || got.JobID != "" {
		t.Fatalf("stage b = %+v, want pending with no job", got)
	}

	settleStageJob(t, store, stageRow(t, store, run.ID, "a").JobID, "approved", "a ok", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "a"); got.State != StageSucceeded {
		t.Fatalf("stage a = %s, want succeeded", got.State)
	}
	if got := stageRow(t, store, run.ID, "b"); got.State != StageQueued || got.JobID == "" {
		t.Fatalf("stage b = %+v, want queued after a succeeded", got)
	}
	if run.State != RunRunning {
		t.Fatalf("run = %s, want running", run.State)
	}

	settleStageJob(t, store, stageRow(t, store, run.ID, "b").JobID, "implemented", "b ok", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "c"); got.State != StageQueued {
		t.Fatalf("stage c = %s, want queued after b succeeded", got.State)
	}

	settleStageJob(t, store, stageRow(t, store, run.ID, "c").JobID, "approved", "c ok", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)
	if run.State != RunSucceeded {
		t.Fatalf("run = %s, want succeeded", run.State)
	}
	if got := stageRow(t, store, run.ID, "c"); got.State != StageSucceeded {
		t.Fatalf("stage c = %s, want succeeded", got.State)
	}
	// The pipeline row mirrors the terminal status.
	if rec2, _, _ := store.GetPipeline(context.Background(), "chain"); rec2.LastStatus != RunSucceeded {
		t.Fatalf("pipeline last_status = %q, want succeeded", rec2.LastStatus)
	}
}

const diamondSpec = `name: diamond
repo: owner/repo
stages:
  - id: a
    cmd: echo a
  - id: b
    cmd: echo b
    needs: [a]
  - id: c
    cmd: echo c
    needs: [a]
  - id: d
    cmd: echo d
    needs: [b, c]
`

// TestAdvancerDiamond proves fan-out/fan-in: a fans out to b AND c, and d waits
// for BOTH before it is enqueued.
func TestAdvancerDiamond(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "diamond", diamondSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	settleStageJob(t, store, stageRow(t, store, run.ID, "a").JobID, "approved", "a", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)
	// Fan-out: both b and c enqueue off a.
	if got := stageRow(t, store, run.ID, "b"); got.State != StageQueued {
		t.Fatalf("stage b = %s, want queued", got.State)
	}
	if got := stageRow(t, store, run.ID, "c"); got.State != StageQueued {
		t.Fatalf("stage c = %s, want queued", got.State)
	}
	if got := stageRow(t, store, run.ID, "d"); got.State != StagePending {
		t.Fatalf("stage d = %s, want pending", got.State)
	}

	// Only b succeeds: d must NOT enqueue (c still in flight).
	settleStageJob(t, store, stageRow(t, store, run.ID, "b").JobID, "approved", "b", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "d"); got.State != StagePending {
		t.Fatalf("stage d = %s, want still pending (c not done)", got.State)
	}
	if run.State != RunRunning {
		t.Fatalf("run = %s, want running", run.State)
	}

	// c succeeds: now d is ready.
	settleStageJob(t, store, stageRow(t, store, run.ID, "c").JobID, "approved", "c", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "d"); got.State != StageQueued {
		t.Fatalf("stage d = %s, want queued (both deps done)", got.State)
	}

	settleStageJob(t, store, stageRow(t, store, run.ID, "d").JobID, "approved", "d", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)
	if run.State != RunSucceeded {
		t.Fatalf("run = %s, want succeeded", run.State)
	}
}

func TestAdvancerFastStageUsesJobEventTimes(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	enqueueAt := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	startedAt := enqueueAt.Add(17 * time.Second)
	finishedAt := startedAt.Add(23 * time.Second)
	foldAt := enqueueAt.Add(2 * time.Minute)

	run := startTestRun(t, store, rec, spec, enqueue, enqueueAt)
	jobID := stageRow(t, store, run.ID, "a").JobID
	startStageJob(t, store, jobID)
	setPipelineJobEventTime(t, store, jobID, string(workflow.JobRunning), startedAt)
	settleStageJob(t, store, jobID, "approved", "fast", nil)
	setPipelineJobEventTime(t, store, jobID, string(workflow.JobSucceeded), finishedAt)

	run = advance(t, store, rec, spec, enqueue, run, foldAt)
	got := stageRow(t, store, run.ID, "a")
	if !got.StartedAt.Equal(startedAt) || !got.FinishedAt.Equal(finishedAt) {
		t.Fatalf("fast stage times = (%s, %s), want job events (%s, %s)", got.StartedAt, got.FinishedAt, startedAt, finishedAt)
	}
	if got.StartedAt.Equal(enqueueAt) || got.FinishedAt.Equal(foldAt) {
		t.Fatalf("fast stage retained advancer ticks: %+v", got)
	}
}

func TestAdvancerStageUsesEarliestRunningEventAcrossReclaim(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	enqueueAt := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	firstStartedAt := enqueueAt.Add(time.Minute)
	reclaimedAt := enqueueAt.Add(16 * time.Minute)
	finishedAt := enqueueAt.Add(21 * time.Minute)
	run := startTestRun(t, store, rec, spec, enqueue, enqueueAt)
	jobID := stageRow(t, store, run.ID, "a").JobID

	startStageJob(t, store, jobID)
	ok, err := store.TransitionJobStateWithEvent(context.Background(), jobID, string(workflow.JobRunning), string(workflow.JobQueued), db.JobEvent{
		JobID: jobID, Kind: string(workflow.JobQueued), Message: "requeued after reboot by test",
	})
	if err != nil || !ok {
		t.Fatalf("requeue %s: ok=%v err=%v", jobID, ok, err)
	}
	startStageJob(t, store, jobID)
	setPipelineJobEventTimeAtIndex(t, store, jobID, string(workflow.JobRunning), 0, firstStartedAt)
	setPipelineJobEventTimeAtIndex(t, store, jobID, string(workflow.JobRunning), 1, reclaimedAt)
	settleStageJob(t, store, jobID, "approved", "survived reclaim", nil)
	setPipelineJobEventTime(t, store, jobID, string(workflow.JobSucceeded), finishedAt)

	run = advance(t, store, rec, spec, enqueue, run, finishedAt.Add(time.Minute))
	got := stageRow(t, store, run.ID, "a")
	if !got.StartedAt.Equal(firstStartedAt) || !got.FinishedAt.Equal(finishedAt) {
		t.Fatalf("reclaimed stage times = (%s, %s), want earliest start %s and finish %s", got.StartedAt, got.FinishedAt, firstStartedAt, finishedAt)
	}
	if duration := got.FinishedAt.Sub(got.StartedAt); duration != 20*time.Minute {
		t.Fatalf("reclaimed stage duration = %s, want 20m (not 5m from reclaim)", duration)
	}
}

func TestAdvancerForkedStagesUseDistinctJobEventTimes(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "fork", independentBranchSpec)
	tick := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, spec, enqueue, tick)

	want := map[string][2]time.Time{
		"a": {tick.Add(10 * time.Second), tick.Add(20 * time.Second)},
		"b": {tick.Add(30 * time.Second), tick.Add(50 * time.Second)},
	}
	for stageID, times := range want {
		jobID := stageRow(t, store, run.ID, stageID).JobID
		startStageJob(t, store, jobID)
		setPipelineJobEventTime(t, store, jobID, string(workflow.JobRunning), times[0])
		settleStageJob(t, store, jobID, "approved", stageID+" done", nil)
		setPipelineJobEventTime(t, store, jobID, string(workflow.JobSucceeded), times[1])
	}

	run = advance(t, store, rec, spec, enqueue, run, tick.Add(5*time.Minute))
	for stageID, times := range want {
		got := stageRow(t, store, run.ID, stageID)
		if !got.StartedAt.Equal(times[0]) || !got.FinishedAt.Equal(times[1]) {
			t.Fatalf("stage %s times = (%s, %s), want (%s, %s)", stageID, got.StartedAt, got.FinishedAt, times[0], times[1])
		}
	}
}

func TestAdvancerPreClaimFailureHasNonNegativeDuration(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	tick := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, spec, enqueue, tick)
	jobID := stageRow(t, store, run.ID, "a").JobID
	terminalAt := tick.Add(45 * time.Second)

	settleStageJob(t, store, jobID, "failed", "failed before claim", nil)
	setPipelineJobEventTime(t, store, jobID, string(workflow.JobFailed), terminalAt)
	run = advance(t, store, rec, spec, enqueue, run, tick.Add(time.Minute))
	got := stageRow(t, store, run.ID, "a")
	if got.State != StageFailed || !got.StartedAt.Equal(terminalAt) || !got.FinishedAt.Equal(terminalAt) {
		t.Fatalf("pre-claim failure stage = %+v, want terminal with zero non-negative duration at %s", got, terminalAt)
	}
}

// TestAdvancerBlockedPark proves a blocked stage parks the run: needs are
// persisted at stage AND run level, and the downstream stage is never enqueued.
func TestAdvancerBlockedPark(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	settleStageJob(t, store, stageRow(t, store, run.ID, "a").JobID, "approved", "a ok", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)

	settleStageJob(t, store, stageRow(t, store, run.ID, "b").JobID, "blocked", "needs a secret", []string{"R2 token"})
	run = advance(t, store, rec, spec, enqueue, run, now)

	if run.State != RunBlocked {
		t.Fatalf("run = %s, want blocked", run.State)
	}
	if run.HaltStage != "b" {
		t.Fatalf("halt_stage = %q, want b", run.HaltStage)
	}
	if got := decodePipelineNeeds(run.NeedsJSON); len(got) != 1 || got[0] != "R2 token" {
		t.Fatalf("run needs = %v, want [R2 token]", got)
	}
	b := stageRow(t, store, run.ID, "b")
	if b.State != StageBlocked {
		t.Fatalf("stage b = %s, want blocked", b.State)
	}
	if got := decodePipelineNeeds(b.NeedsJSON); len(got) != 1 || got[0] != "R2 token" {
		t.Fatalf("stage b needs = %v, want [R2 token]", got)
	}
	c := stageRow(t, store, run.ID, "c")
	if c.JobID != "" {
		t.Fatalf("stage c job = %q, want never enqueued", c.JobID)
	}
	if c.State != StageSkipped {
		t.Fatalf("stage c = %s, want skipped", c.State)
	}
}

const independentBranchSpec = `name: fork
repo: owner/repo
stages:
  - id: a
    cmd: echo a
  - id: b
    cmd: echo b
  - id: c
    cmd: echo c
    needs: [b]
`

// TestAdvancerIndependentBranchRunsWhenSiblingBlocks proves per-branch (not
// run-wide) halting: two independent roots a and b, with c depending only on b.
// When a blocks, c's branch (b -> c) is fully reachable and MUST still run to
// completion; the run parks blocked only because of a, and only once nothing is in
// flight. A run-wide fail-fast would wrongly skip c even though its dep b succeeded.
func TestAdvancerIndependentBranchRunsWhenSiblingBlocks(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "fork", independentBranchSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	// Both roots enqueue immediately; c waits on b.
	run := startTestRun(t, store, rec, spec, enqueue, now)
	if got := stageRow(t, store, run.ID, "a"); got.State != StageQueued {
		t.Fatalf("stage a = %s, want queued", got.State)
	}
	if got := stageRow(t, store, run.ID, "b"); got.State != StageQueued {
		t.Fatalf("stage b = %s, want queued", got.State)
	}

	// a blocks, b succeeds: the block must NOT halt b's independent branch.
	settleStageJob(t, store, stageRow(t, store, run.ID, "a").JobID, "blocked", "a needs a secret", []string{"R2 token"})
	settleStageJob(t, store, stageRow(t, store, run.ID, "b").JobID, "approved", "b ok", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)

	// c's dep b succeeded, so c is enqueued and the run stays running (c in flight).
	c := stageRow(t, store, run.ID, "c")
	if c.State != StageQueued || c.JobID == "" {
		t.Fatalf("stage c = %+v, want queued after b succeeded (independent of blocked a)", c)
	}
	if run.State != RunRunning {
		t.Fatalf("run = %s, want still running while c is in flight", run.State)
	}
	if got := stageRow(t, store, run.ID, "a"); got.State != StageBlocked {
		t.Fatalf("stage a = %s, want blocked", got.State)
	}

	// c completes: only now, with nothing in flight, does the run park blocked on a.
	settleStageJob(t, store, c.JobID, "approved", "c ok", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "c"); got.State != StageSucceeded {
		t.Fatalf("stage c = %s, want succeeded (reachable branch ran to completion)", got.State)
	}
	if run.State != RunBlocked {
		t.Fatalf("run = %s, want blocked (parks on a once nothing is in flight)", run.State)
	}
	if run.HaltStage != "a" {
		t.Fatalf("halt_stage = %q, want a", run.HaltStage)
	}
	if got := decodePipelineNeeds(run.NeedsJSON); len(got) != 1 || got[0] != "R2 token" {
		t.Fatalf("run needs = %v, want [R2 token]", got)
	}
}

const retrySpec = `name: retry
repo: owner/repo
stages:
  - id: a
    cmd: echo a
    retry: 1
  - id: b
    cmd: echo b
    needs: [a]
`

// TestAdvancerFailedRetryBudget proves a failed stage is retried while its budget
// lasts (a fresh attempt/job id), then parks the run failed once exhausted.
func TestAdvancerFailedRetryBudget(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "retry", retrySpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	firstJob := stageRow(t, store, run.ID, "a").JobID

	// First failure: budget (retry:1) remains, so a re-attempts under a new id.
	settleStageJob(t, store, firstJob, "failed", "boom", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)
	a := stageRow(t, store, run.ID, "a")
	if a.State != StageQueued {
		t.Fatalf("stage a = %s, want re-queued after retry", a.State)
	}
	if a.Attempt != 1 {
		t.Fatalf("stage a attempt = %d, want 1", a.Attempt)
	}
	if a.JobID == firstJob || a.JobID == "" {
		t.Fatalf("retry job id = %q, want a fresh id (was %q)", a.JobID, firstJob)
	}
	if run.State != RunRunning {
		t.Fatalf("run = %s, want still running during retry", run.State)
	}

	// Second failure: budget exhausted, run parks failed and b is skipped.
	settleStageJob(t, store, a.JobID, "failed", "boom again", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)
	if run.State != RunFailed {
		t.Fatalf("run = %s, want failed", run.State)
	}
	if run.HaltStage != "a" {
		t.Fatalf("halt_stage = %q, want a", run.HaltStage)
	}
	if got := stageRow(t, store, run.ID, "a"); got.State != StageFailed || got.Attempt != 1 {
		t.Fatalf("stage a = %+v, want failed attempt 1", got)
	}
	if got := stageRow(t, store, run.ID, "b"); got.State != StageSkipped || got.JobID != "" {
		t.Fatalf("stage b = %+v, want skipped never-enqueued", got)
	}
}

func TestAdvancerRetryResetsAndRederivesAttemptTimes(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "retry", retrySpec)
	tick := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, spec, enqueue, tick)
	firstJob := stageRow(t, store, run.ID, "a").JobID

	startStageJob(t, store, firstJob)
	settleStageJob(t, store, firstJob, "failed", "retry me", nil)
	failingEnqueue := func(context.Context, workflow.JobRequest) (db.Job, error) {
		return db.Job{}, errors.New("pause after retry reset")
	}
	if _, err := AdvancePipelineRun(context.Background(), store, failingEnqueue, rec, spec, run, tick.Add(time.Minute)); err == nil {
		t.Fatal("advance with failing retry enqueue returned nil error")
	}
	reset := stageRow(t, store, run.ID, "a")
	if reset.State != StagePending || reset.JobID != "" || !reset.StartedAt.IsZero() || !reset.FinishedAt.IsZero() {
		t.Fatalf("retry reset stage = %+v, want pending with both timestamps zero", reset)
	}

	run = advance(t, store, rec, spec, enqueue, run, tick.Add(2*time.Minute))
	second := stageRow(t, store, run.ID, "a")
	if second.JobID == "" || second.JobID == firstJob {
		t.Fatalf("retry job id = %q, want new id after %q", second.JobID, firstJob)
	}
	startedAt := tick.Add(3 * time.Minute)
	finishedAt := tick.Add(4 * time.Minute)
	startStageJob(t, store, second.JobID)
	setPipelineJobEventTime(t, store, second.JobID, string(workflow.JobRunning), startedAt)
	run = advance(t, store, rec, spec, enqueue, run, startedAt.Add(10*time.Second))
	if live := stageRow(t, store, run.ID, "a"); live.State != StageRunning || !live.StartedAt.Equal(startedAt) {
		t.Fatalf("live retry stage = %+v, want real running-event start %s", live, startedAt)
	}
	settleStageJob(t, store, second.JobID, "approved", "retry passed", nil)
	setPipelineJobEventTime(t, store, second.JobID, string(workflow.JobSucceeded), finishedAt)
	run = advance(t, store, rec, spec, enqueue, run, tick.Add(10*time.Minute))
	got := stageRow(t, store, run.ID, "a")
	if !got.StartedAt.Equal(startedAt) || !got.FinishedAt.Equal(finishedAt) {
		t.Fatalf("retried stage times = (%s, %s), want new job events (%s, %s)", got.StartedAt, got.FinishedAt, startedAt, finishedAt)
	}
}

const changesRequestedSpec = `name: cr
repo: owner/repo
stages:
  - id: a
    cmd: echo a
  - id: b
    cmd: echo b
    needs: [a]
`

// TestAdvancerChangesRequestedDoesNotAdvance is the stateForDecision TRAP: a
// changes_requested stage job is jobs.state=SUCCEEDED, but changes_requested is
// NOT a default success_decision, so the advancer must treat the stage as failed
// (not advance). Folding on jobs.state would wrongly succeed it and enqueue b.
func TestAdvancerChangesRequestedDoesNotAdvance(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "cr", changesRequestedSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	aJob := stageRow(t, store, run.ID, "a").JobID
	settleStageJob(t, store, aJob, "changes_requested", "needs work", nil)

	// Guard the trap premise: the underlying JOB really is succeeded.
	if job, _ := store.GetJob(context.Background(), aJob); job.State != string(workflow.JobSucceeded) {
		t.Fatalf("precondition: job state = %s, want succeeded (the trap)", job.State)
	}

	run = advance(t, store, rec, spec, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "a"); got.State != StageFailed {
		t.Fatalf("stage a = %s, want failed (changes_requested must NOT advance)", got.State)
	}
	if got := stageRow(t, store, run.ID, "b"); got.JobID != "" || got.State == StageQueued {
		t.Fatalf("stage b = %+v, want never enqueued", got)
	}
	if run.State != RunFailed {
		t.Fatalf("run = %s, want failed", run.State)
	}
}

// TestAdvancerChangesRequestedHonorsSuccessOverride proves the same
// changes_requested decision DOES advance when the stage opts it into
// success_decisions — the decision, not jobs.state, is what the advancer keys on.
func TestAdvancerChangesRequestedHonorsSuccessOverride(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	const spec = `name: cr2
repo: owner/repo
stages:
  - id: a
    cmd: echo a
    success_decisions: [approved, changes_requested]
  - id: b
    cmd: echo b
    needs: [a]
`
	rec, parsed := newTestPipeline(t, store, "cr2", spec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, parsed, enqueue, now)
	settleStageJob(t, store, stageRow(t, store, run.ID, "a").JobID, "changes_requested", "ok enough", nil)
	run = advance(t, store, rec, parsed, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "a"); got.State != StageSucceeded {
		t.Fatalf("stage a = %s, want succeeded (override lists changes_requested)", got.State)
	}
	if got := stageRow(t, store, run.ID, "b"); got.State != StageQueued {
		t.Fatalf("stage b = %s, want queued", got.State)
	}
}

// TestAdvancerSkippedAdvancesByDefault proves an honest no-work result is a
// first-class successful fold: the existing StageSucceeded row carries the trusted
// marker and the dependent stage enqueues with zero success_decisions config.
func TestAdvancerSkippedAdvancesByDefault(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "skip-default", changesRequestedSpec)
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	settleStageJob(t, store, stageRow(t, store, run.ID, "a").JobID, "skipped", "no new replies", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)

	a := stageRow(t, store, run.ID, "a")
	if a.State != StageSucceeded {
		t.Fatalf("stage a = %s, want succeeded", a.State)
	}
	if a.Summary != "[skipped: no work] no new replies" {
		t.Fatalf("stage a summary = %q", a.Summary)
	}
	if got := stageRow(t, store, run.ID, "b"); got.State != StageQueued || got.JobID == "" {
		t.Fatalf("stage b = %+v, want queued with a job", got)
	}
}

// TestAdvancerSkippedHonorsStrictSuccessDecisions proves an explicit list can
// require real work: omitting skipped makes the no-work decision fail the stage.
func TestAdvancerSkippedHonorsStrictSuccessDecisions(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	const spec = `name: skip-strict
repo: owner/repo
success_decisions: [approved, implemented]
stages:
  - id: a
    cmd: echo a
  - id: b
    cmd: echo b
    needs: [a]
`
	rec, parsed := newTestPipeline(t, store, "skip-strict", spec)
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, parsed, enqueue, now)
	settleStageJob(t, store, stageRow(t, store, run.ID, "a").JobID, "skipped", "no new replies", nil)
	run = advance(t, store, rec, parsed, enqueue, run, now)

	if got := stageRow(t, store, run.ID, "a"); got.State != StageFailed {
		t.Fatalf("stage a = %s, want failed when strict list omits skipped", got.State)
	}
	if got := stageRow(t, store, run.ID, "b"); got.State != StageSkipped || got.JobID != "" {
		t.Fatalf("stage b = %+v, want skipped and never enqueued", got)
	}
	if run.State != RunFailed {
		t.Fatalf("run = %s, want failed", run.State)
	}
}

// TestAdvancerStripsForgedSkippedMarkerFromNonSkippedOutcomes proves failed and
// blocked folds cannot persist Gitmoot's reserved no-work provenance marker.
func TestAdvancerStripsForgedSkippedMarkerFromNonSkippedOutcomes(t *testing.T) {
	cases := []struct {
		decision string
		summary  string
		want     string
	}{
		{"blocked", pipelineSkippedSummaryMarker, "stage blocked"},
		{"failed", pipelineSkippedSummaryMarker + pipelineSkippedSummaryMarker + " boom", "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.decision, func(t *testing.T) {
			store := pipelineAdvanceStore(t)
			enqueue := testStageEnqueuer(store)
			rec, spec := newTestPipeline(t, store, "forged-"+tc.decision, changesRequestedSpec)
			now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
			run := startTestRun(t, store, rec, spec, enqueue, now)
			settleStageJob(t, store, stageRow(t, store, run.ID, "a").JobID, tc.decision, tc.summary, []string{"operator input"})

			run = advance(t, store, rec, spec, enqueue, run, now)
			got := stageRow(t, store, run.ID, "a")
			if got.Summary != tc.want {
				t.Fatalf("persisted summary = %q, want %q", got.Summary, tc.want)
			}
			if strings.HasPrefix(got.Summary, pipelineSkippedSummaryMarker) {
				t.Fatalf("persisted summary retained forged skipped marker: %q", got.Summary)
			}
		})
	}
}

// TestAdvancerSkippedPropagation proves a failed stage's transitive dependents are
// all marked skipped (never enqueued), giving a clean terminal picture.
func TestAdvancerSkippedPropagation(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	// a fails with no retry budget -> b and c can never run.
	settleStageJob(t, store, stageRow(t, store, run.ID, "a").JobID, "failed", "root broke", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)

	if run.State != RunFailed {
		t.Fatalf("run = %s, want failed", run.State)
	}
	for _, id := range []string{"b", "c"} {
		got := stageRow(t, store, run.ID, id)
		if got.State != StageSkipped || got.JobID != "" {
			t.Fatalf("stage %s = %+v, want skipped never-enqueued", id, got)
		}
	}
}

// TestAdvancerIdempotentRescan proves a re-scan with nothing newly settled makes
// no new enqueues and no state change: the deterministic ids + stage-state gating
// make advancement safe to run every tick.
func TestAdvancerIdempotentRescan(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "chain", linearChainSpec)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, spec, enqueue, now)
	beforeJobs := countRunJobs(t, store, run.ID)
	beforeA := stageRow(t, store, run.ID, "a")

	// Re-advance with no settled job: pure no-op.
	run = advance(t, store, rec, spec, enqueue, run, now)
	run = advance(t, store, rec, spec, enqueue, run, now)
	if got := countRunJobs(t, store, run.ID); got != beforeJobs {
		t.Fatalf("re-scan enqueued jobs: before=%d after=%d", beforeJobs, got)
	}
	afterA := stageRow(t, store, run.ID, "a")
	if afterA.State != beforeA.State || afterA.JobID != beforeA.JobID {
		t.Fatalf("re-scan changed stage a: before=%+v after=%+v", beforeA, afterA)
	}
	if run.State != RunRunning {
		t.Fatalf("run = %s, want still running", run.State)
	}

	// Advance one real layer, then prove the settled layer is also idempotent.
	settleStageJob(t, store, afterA.JobID, "approved", "a", nil)
	run = advance(t, store, rec, spec, enqueue, run, now)
	midJobs := countRunJobs(t, store, run.ID)
	bJob := stageRow(t, store, run.ID, "b").JobID
	run = advance(t, store, rec, spec, enqueue, run, now)
	if got := countRunJobs(t, store, run.ID); got != midJobs {
		t.Fatalf("re-scan after a-settled enqueued jobs: before=%d after=%d", midJobs, got)
	}
	if got := stageRow(t, store, run.ID, "b").JobID; got != bJob {
		t.Fatalf("re-scan changed stage b job id: before=%q after=%q", bJob, got)
	}
}

// TestPipelineStageSettleOutcomeDefaultMatchesFold pins the settle-predicate seam
// (stageSettleOutcome) for today's kinds: (1) a non-terminal stage job is NOT
// settled, so the FOLD pass leaves it in flight; (2) once the job settles, the seam
// reports settled AND returns EXACTLY what foldPipelineStageOutcome returns for the
// same job — for a shell stage AND a read-only agent stage, across a success and a
// blocked/needs decision. This is the byte-identity contract a future gate/orchestrate
// kind must not disturb for existing kinds.
func TestPipelineStageSettleOutcomeDefaultMatchesFold(t *testing.T) {
	const settleSpec = `name: settle
repo: owner/repo
stages:
  - id: sh
    cmd: echo hi
  - id: ag
    agent: asker
    prompt: What changed?
  - id: data
    agent: producer
    action: produce
    prompt: Write data.
    write: true
    writes: [/tmp/gitmoot-produce-test]
`
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	rec, spec := newTestPipeline(t, store, "settle", settleSpec)
	now := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, spec, enqueue, now)
	ctx := context.Background()

	specByID := make(map[string]Stage, len(spec.Stages))
	for _, s := range spec.Stages {
		specByID[s.ID] = s
	}

	cases := []struct {
		stageID, decision, summary string
		needs                      []string
	}{
		{"sh", "approved", "shell landed", nil},
		{"ag", "blocked", "needs a hint", []string{"which module?"}},
		{"data", "skipped", "no batch", nil},
	}
	for _, tc := range cases {
		row := stageRow(t, store, run.ID, tc.stageID)
		if row.JobID == "" {
			t.Fatalf("%s: stage not enqueued (job id empty)", tc.stageID)
		}
		stage := specByID[tc.stageID]

		// The seam loads the job itself via deps.store, so a jobless future kind is
		// never handed a fabricated job; today's kinds read only the store.
		deps := pipelineStageSettleDeps{store: store, rec: rec, run: run, now: now}

		// (1) Before settling, the job is queued/running: the seam reports unsettled,
		// matching the inline IsSettledJobState guard the FOLD pass used to run.
		pending, err := store.GetJob(ctx, row.JobID)
		if err != nil {
			t.Fatalf("GetJob(%s): %v", tc.stageID, err)
		}
		if settled, _, _, _, _, err := stageSettleOutcome(ctx, deps, spec, stage, row); err != nil || settled {
			t.Fatalf("%s: seam settled a non-terminal job (state=%q, err=%v)", tc.stageID, pending.State, err)
		}

		// (2) After settling, the seam must settle and return the identical fold.
		settleStageJob(t, store, row.JobID, tc.decision, tc.summary, tc.needs)
		job, err := store.GetJob(ctx, row.JobID)
		if err != nil {
			t.Fatalf("GetJob(%s) after settle: %v", tc.stageID, err)
		}
		wantState, wantSummary, wantNeeds := foldPipelineStageOutcome(spec.EffectiveSuccessDecisions(stage), job)
		settled, gotState, gotSummary, gotNeeds, _, err := stageSettleOutcome(ctx, deps, spec, stage, row)
		if err != nil {
			t.Fatalf("%s: seam returned err on terminal job: %v", tc.stageID, err)
		}
		if !settled {
			t.Fatalf("%s: seam did not settle a terminal job (state=%q)", tc.stageID, job.State)
		}
		if gotState != wantState || gotSummary != wantSummary || !reflect.DeepEqual(gotNeeds, wantNeeds) {
			t.Fatalf("%s: seam (%q,%q,%v) != fold (%q,%q,%v)", tc.stageID, gotState, gotSummary, gotNeeds, wantState, wantSummary, wantNeeds)
		}
	}
}

// countRunJobs counts persisted jobs belonging to a run (root == run id).
func countRunJobs(t *testing.T, store *db.Store, runID string) int {
	t.Helper()
	jobs, err := store.ListJobsByRoot(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListJobsByRoot: %v", err)
	}
	return len(jobs)
}
