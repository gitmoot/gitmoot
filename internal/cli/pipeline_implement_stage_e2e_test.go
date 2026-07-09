package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
	"github.com/jerryfane/gitmoot/internal/runtime"
	"github.com/jerryfane/gitmoot/internal/workflow"
)

// settleImplementStageJob transitions a stage's enqueued job to a terminal state with
// the given decision AND an opened PR number, faithfully reproducing what the real
// implement flow persists once the ImplementationFinalizer stamps the PR onto the
// payload. A pr of 0 models the RACE the #768 settle guard closes: the job is terminal
// SUCCESS but the PR is not stamped yet.
func settleImplementStageJob(t *testing.T, store *db.Store, jobID, decision, summary string, pr int) {
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
	payload.Result = &workflow.AgentResult{Decision: decision, Summary: summary}
	payload.PullRequest = pr
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

// stampImplementStageJobPR sets an already-terminal implement stage job's PullRequest
// on the payload WITHOUT re-transitioning it — simulating the finalizer stamping the
// opened PR a beat after the job settled (the race the settle guard waits out).
func stampImplementStageJobPR(t *testing.T, store *db.Store, jobID string, pr int) {
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
	payload.PullRequest = pr
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, jobID, string(encoded)); err != nil {
		t.Fatalf("UpdateJobPayload: %v", err)
	}
}

// TestPipelineImplementStagePRGateAndDownstreamE2E proves the #768 Model A settle
// semantics for a MUTATING implement stage, end to end through the real advancer:
//
//  1. FOLD-ON-PR-OPENED gate: the implement job settles SUCCESS with decision
//     "implemented" but NO PR stamped yet — the stage must stay in flight (NOT
//     succeeded), closing the race where a downstream stage would advance believing a
//     PR exists. Once the PR is stamped, the same stage folds succeeded.
//  2. The implement stage job binds to the NAMED agent with the DETERMINISTIC,
//     attempt-independent branch/task derived from run+stage.
//  3. DOWNSTREAM CONSUMPTION: a stage that needs the implement stage receives the
//     opened PR number in its prompt via the #757 upstream-context injection.
//
// It uses the real store-backed advancer (no worker/finalizer, so it is deterministic
// and offline) — the implement job's terminal state + payload are set directly, exactly
// what the finalizer would persist.
func TestPipelineImplementStagePRGateAndDownstreamE2E(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	const spec = `name: impl-gate
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: notify
    agent: notifier
    prompt: Announce the change.
    needs: [impl]
`
	rec, parsed := newTestPipeline(t, store, "impl-gate", spec)
	now := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)
	ctx := context.Background()

	run := startTestRun(t, store, rec, parsed, enqueue, now)

	// The implement stage enqueued with the deterministic branch/task (attempt 0).
	impl := stageRow(t, store, run.ID, "impl")
	if impl.State != pipeline.StageQueued || impl.JobID == "" {
		t.Fatalf("impl stage = %+v, want queued with a job", impl)
	}
	implJob, err := store.GetJob(ctx, impl.JobID)
	if err != nil {
		t.Fatalf("GetJob(impl): %v", err)
	}
	if implJob.Type != "implement" {
		t.Fatalf("impl job action = %q, want implement", implJob.Type)
	}
	implPayload, err := workflow.ParseJobPayload(implJob.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(impl): %v", err)
	}
	wantBranch := "gitmoot/pipe-" + run.ID + "-impl"
	wantTask := "pipe-" + run.ID + "-impl"
	if implPayload.Branch != wantBranch {
		t.Fatalf("impl branch = %q, want %q", implPayload.Branch, wantBranch)
	}
	if implPayload.TaskID != wantTask {
		t.Fatalf("impl task = %q, want %q", implPayload.TaskID, wantTask)
	}

	// (1) The job settles SUCCESS but with NO PR stamped: the gate holds the stage in
	// flight (still queued), NOT succeeded, and the run stays running.
	settleImplementStageJob(t, store, impl.JobID, "implemented", "landed the fix", 0)
	run = advance(t, store, rec, parsed, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "impl"); got.State != pipeline.StageQueued {
		t.Fatalf("impl stage = %s, want still queued (PR-gate holds an unstamped success)", got.State)
	}
	if run.State != pipeline.RunRunning {
		t.Fatalf("run = %s, want running while impl waits on its PR", run.State)
	}
	if got := stageRow(t, store, run.ID, "notify"); got.JobID != "" {
		t.Fatalf("notify enqueued before impl succeeded: %+v", got)
	}

	// The PR is stamped a beat later: NOW the stage folds succeeded and notify enqueues.
	stampImplementStageJobPR(t, store, impl.JobID, 42)
	run = advance(t, store, rec, parsed, enqueue, run, now)
	implDone := stageRow(t, store, run.ID, "impl")
	if implDone.State != pipeline.StageSucceeded {
		t.Fatalf("impl stage = %s, want succeeded once PR stamped", implDone.State)
	}
	if !strings.Contains(implDone.Summary, "opened PR #42") {
		t.Fatalf("impl summary = %q, want it to carry the opened PR", implDone.Summary)
	}

	// (3) The downstream notify stage's enqueued prompt carries the implement stage's
	// summary INCLUDING the opened PR, via the #757 upstream-context injection.
	notify := stageRow(t, store, run.ID, "notify")
	if notify.State != pipeline.StageQueued || notify.JobID == "" {
		t.Fatalf("notify stage = %+v, want queued with a job after impl succeeded", notify)
	}
	notifyJob, err := store.GetJob(ctx, notify.JobID)
	if err != nil {
		t.Fatalf("GetJob(notify): %v", err)
	}
	notifyPayload, err := workflow.ParseJobPayload(notifyJob.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(notify): %v", err)
	}
	if !strings.Contains(notifyPayload.Instructions, "Upstream stage results") {
		t.Fatalf("notify prompt missing upstream-context block:\n%s", notifyPayload.Instructions)
	}
	if !strings.Contains(notifyPayload.Instructions, "opened PR #42") {
		t.Fatalf("notify prompt missing the upstream PR reference:\n%s", notifyPayload.Instructions)
	}
	if !strings.Contains(notifyPayload.Instructions, "Announce the change.") {
		t.Fatalf("notify prompt lost its own task text:\n%s", notifyPayload.Instructions)
	}

	// The run completes once notify approves.
	settleStageJob(t, store, notify.JobID, "approved", "announced", nil)
	run = advance(t, store, rec, parsed, enqueue, run, now)
	if run.State != pipeline.RunSucceeded {
		t.Fatalf("run = %s, want succeeded", run.State)
	}
}

// TestPipelineImplementStageNoOpTerminalFoldsE2E proves the settle guard does NOT wedge
// forever on the ROUTINE no-op path: an implement agent that produces no diff settles
// with decision "implemented" but PullRequest=0, and the engine records the terminal
// "advance_skipped_no_pr" job event (engine.go). That event is the definitive signal that
// no PR will ever land, so the stage must fold SUCCEEDED (no PR marker) rather than hold
// in flight indefinitely, letting the downstream stage advance.
func TestPipelineImplementStageNoOpTerminalFoldsE2E(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	const spec = `name: noop-flow
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: notify
    agent: rev
    prompt: Announce.
    needs: [impl]
`
	rec, parsed := newTestPipeline(t, store, "noop-flow", spec)
	now := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, parsed, enqueue, now)
	impl := stageRow(t, store, run.ID, "impl")

	// The job settles SUCCESS with NO PR (a no-op change) — but BEFORE the engine's
	// terminal marker exists, the stage still holds in flight (the finalizer-race path).
	settleImplementStageJob(t, store, impl.JobID, "implemented", "no changes needed", 0)
	run = advance(t, store, rec, parsed, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "impl"); got.State != pipeline.StageQueued {
		t.Fatalf("impl stage = %s, want still queued before the no-pr marker exists", got.State)
	}

	// The engine records the terminal "advance_skipped_no_pr" event: NOW the stage folds
	// succeeded (no PR marker) and the downstream notify stage enqueues.
	if err := store.AddJobEvent(ctx, db.JobEvent{JobID: impl.JobID, Kind: "advance_skipped_no_pr", Message: "no pull request is attached"}); err != nil {
		t.Fatalf("AddJobEvent: %v", err)
	}
	run = advance(t, store, rec, parsed, enqueue, run, now)
	implDone := stageRow(t, store, run.ID, "impl")
	if implDone.State != pipeline.StageSucceeded {
		t.Fatalf("impl stage = %s, want succeeded once the no-pr marker exists", implDone.State)
	}
	if strings.Contains(implDone.Summary, "opened PR") {
		t.Fatalf("impl summary = %q, want no opened-PR marker for a no-op change", implDone.Summary)
	}
	if got := stageRow(t, store, run.ID, "notify"); got.State != pipeline.StageQueued || got.JobID == "" {
		t.Fatalf("notify stage = %+v, want queued with a job after impl succeeded", got)
	}
}

// TestPipelineImplementStageWritableWorktreeReuseE2E proves the WRITABLE-worktree
// dispatch (#768): a mutating implement stage takes the real task-worktree path (not
// the read-only committed-tip worktree), is born on its DETERMINISTIC run-scoped branch,
// keys worktree:<path> (so mutating same-repo stages parallelize), and a RETRY reuses
// the SAME branch/worktree and FAILS CLOSED when that worktree has uncommitted changes.
//
// It drives the production enqueuer (newPipelineStageEnqueuer) against a real local git
// checkout so the writable allocation genuinely runs, but never runs the worker (the
// implement finalizer needs GitHub) — the retry is driven by settling the job failed.
func TestPipelineImplementStageWritableWorktreeReuseE2E(t *testing.T) {
	ctx := context.Background()
	home, _, store := heartbeatLoopE2EHome(t)

	checkout := createDaemonWorkerGitCheckout(t, "main")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	// A named implement-capable agent on the shell runtime with a WRITE policy (a
	// read-only policy would fail-closed the implement preflight). The shell body is
	// irrelevant here — the worker never runs; the allocation + retry are what matter.
	seedDaemonWorkerAgentWithPolicy(t, store, "coder", runtime.ShellRuntime,
		pipelineStageResultCmd("implemented", "fixed", nil),
		[]string{"implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)

	const spec = `name: impl-worktree
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
    retry: 1
`
	rec, parsed := newTestPipeline(t, store, "impl-worktree", spec)
	enqueue := newPipelineStageEnqueuer(store, home)
	now := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, parsed, enqueue, now)

	impl := stageRow(t, store, run.ID, "impl")
	if impl.State != pipeline.StageQueued || impl.JobID == "" {
		t.Fatalf("impl stage = %+v, want queued with a job", impl)
	}
	job, err := store.GetJob(ctx, impl.JobID)
	if err != nil {
		t.Fatalf("GetJob(impl): %v", err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload(impl): %v", err)
	}
	// It took the WRITABLE task-worktree, not the read-only committed-tip worktree.
	if strings.TrimSpace(payload.WorktreePath) == "" {
		t.Fatalf("impl stage has no writable task worktree")
	}
	if payload.ReadOnlyWorktree {
		t.Fatalf("impl stage ReadOnlyWorktree = true, want false (a mutating worktree is durable)")
	}
	wantBranch := "gitmoot/pipe-" + run.ID + "-impl"
	if payload.Branch != wantBranch {
		t.Fatalf("impl branch = %q, want %q", payload.Branch, wantBranch)
	}
	if _, statErr := os.Stat(payload.WorktreePath); statErr != nil {
		t.Fatalf("impl worktree %s not created on disk: %v", payload.WorktreePath, statErr)
	}
	// A worktree-keyed job parallelizes with same-repo siblings (never repo:<repo>).
	if key := queuedJobCheckoutKey(ctx, store, job); !strings.HasPrefix(key, "worktree:") {
		t.Fatalf("impl stage checkout key = %q, want worktree:<path> (parallelizes)", key)
	}

	worktreePath := payload.WorktreePath

	// RETRY REUSE + FAIL-CLOSED: dirty the worktree, then fail the job so the stage
	// retries (retry:1). The retry re-derives the SAME deterministic branch/task and
	// reuses the worktree — but the uncommitted change trips the fail-closed guard, so
	// the advance errors rather than duplicating or clobbering the branch/PR.
	if err := os.WriteFile(filepath.Join(worktreePath, "dirty.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatalf("dirty the worktree: %v", err)
	}
	settleStageJob(t, store, impl.JobID, "failed", "boom", nil)
	_, err = advancePipelineRun(ctx, store, enqueue, rec, parsed, run, now)
	if err == nil {
		t.Fatalf("retry into a dirty worktree should FAIL CLOSED, got nil error")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("fail-closed error = %q, want it to mention uncommitted changes", err.Error())
	}
	// The retry stayed on the SAME branch (never minted a fresh one).
	if !strings.Contains(err.Error(), wantBranch) {
		t.Fatalf("fail-closed error = %q, want it to reference the reused branch %q", err.Error(), wantBranch)
	}
	retried := stageRow(t, store, run.ID, "impl")
	if retried.Attempt != 1 {
		t.Fatalf("impl attempt = %d, want 1 (retry budget consumed)", retried.Attempt)
	}
}
