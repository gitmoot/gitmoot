package cli

import (
	"context"
	"testing"
	"time"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/pipeline"
)

// markPipelinePRMerged records (or updates) a PR in the store's pull_requests table
// with the given state — the same row the daemon's PR poller / merge-gate keeps
// current. A gate stage reads merge status from here, so this is how a deterministic
// (offline) test flips the pr_merged predicate without a live GitHub call.
func markPipelinePRMerged(t *testing.T, store *db.Store, repo string, number int64, state string) {
	t.Helper()
	if err := store.UpsertPullRequest(context.Background(), db.PullRequest{
		RepoFullName: repo,
		Number:       number,
		HeadBranch:   "gitmoot/some-branch",
		BaseBranch:   "main",
		State:        state,
	}); err != nil {
		t.Fatalf("UpsertPullRequest(%s#%d, %s): %v", repo, number, state, err)
	}
}

// TestPipelineGateStagePRMergedFoldsAndBlocksE2E proves the #768 Phase 2 JOBLESS gate
// stage end to end through the real advancer:
//
//   - The gate mints NO worker job: once its upstream implement stage succeeds (opening
//     a PR), the gate goes in-flight (queued -> running) with an empty JobID.
//   - While the PR is NOT merged the gate stays in flight, the downstream deploy stage
//     never enqueues, and the run stays running (fail-open: waiting, not a hung poll).
//   - When the PR merges (a merged pull_requests row appears), the SAME gate folds
//     succeeded and the downstream stage enqueues.
//
// It is fully deterministic and offline — the implement job's terminal state + opened
// PR and the PR's merged state are set directly in the store, exactly what the
// finalizer and the PR poller would persist.
func TestPipelineGateStagePRMergedFoldsAndBlocksE2E(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	const spec = `name: gate-flow
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: wait
    gate: pr_merged
    source: impl
    needs: [impl]
    timeout: 1h
  - id: deploy
    cmd: echo deploying
    needs: [wait]
`
	rec, parsed := newTestPipeline(t, store, "gate-flow", spec)
	now := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, parsed, enqueue, now)

	// The implement stage settles success WITH an opened PR (#42), so it folds succeeded
	// and its summary carries "(opened PR #42)".
	impl := stageRow(t, store, run.ID, "impl")
	if impl.State != pipeline.StageQueued || impl.JobID == "" {
		t.Fatalf("impl stage = %+v, want queued with a job", impl)
	}
	settleImplementStageJob(t, store, impl.JobID, "implemented", "landed the fix", 42)
	run = advance(t, store, rec, parsed, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "impl"); got.State != pipeline.StageSucceeded {
		t.Fatalf("impl stage = %s, want succeeded", got.State)
	}

	// The JOBLESS gate is now in-flight (marked queued in the ENQUEUE pass, with NO job).
	gate := stageRow(t, store, run.ID, "wait")
	if gate.State != pipeline.StageQueued {
		t.Fatalf("gate stage = %s, want queued (in-flight, jobless)", gate.State)
	}
	if gate.JobID != "" {
		t.Fatalf("gate stage minted a job %q; a gate must be jobless", gate.JobID)
	}
	if got := stageRow(t, store, run.ID, "deploy"); got.State != pipeline.StagePending {
		t.Fatalf("deploy stage = %s, want pending (gate not satisfied)", got.State)
	}

	// The PR is NOT merged yet: the gate stays in flight (reflecting queued -> running),
	// deploy does not enqueue, and the run stays running.
	run = advance(t, store, rec, parsed, enqueue, run, now)
	gate = stageRow(t, store, run.ID, "wait")
	if gate.State != pipeline.StageRunning {
		t.Fatalf("gate stage = %s, want running while the PR is unmerged", gate.State)
	}
	if gate.JobID != "" {
		t.Fatalf("gate stage acquired a job %q while watching; a gate is jobless", gate.JobID)
	}
	if run.State != pipeline.RunRunning {
		t.Fatalf("run = %s, want running while the gate waits on the merge", run.State)
	}
	if got := stageRow(t, store, run.ID, "deploy"); got.State != pipeline.StagePending {
		t.Fatalf("deploy stage = %s, want still pending while the gate waits", got.State)
	}

	// The PR merges: the gate predicate now holds, so the SAME gate folds succeeded and
	// the downstream deploy stage enqueues.
	markPipelinePRMerged(t, store, "owner/repo", 42, "merged")
	run = advance(t, store, rec, parsed, enqueue, run, now)
	gate = stageRow(t, store, run.ID, "wait")
	if gate.State != pipeline.StageSucceeded {
		t.Fatalf("gate stage = %s, want succeeded once the PR merged", gate.State)
	}
	deploy := stageRow(t, store, run.ID, "deploy")
	if deploy.State != pipeline.StageQueued || deploy.JobID == "" {
		t.Fatalf("deploy stage = %+v, want queued with a job after the gate folded", deploy)
	}

	// The run completes once the downstream deploy stage succeeds.
	settleStageJob(t, store, deploy.JobID, "approved", "deployed", nil)
	run = advance(t, store, rec, parsed, enqueue, run, now)
	if run.State != pipeline.RunSucceeded {
		t.Fatalf("run = %s, want succeeded", run.State)
	}
}

// TestPipelineGateStageRejectsForgedSkippedMarkerE2E proves an implemented source
// cannot forge Gitmoot's reserved no-work marker. The fold strips repeated leading
// markers, appends the trusted PR marker, and the gate observes the real merge.
func TestPipelineGateStageRejectsForgedSkippedMarkerE2E(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	const spec = `name: gate-forged-skip
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: wait
    gate: pr_merged
    source: impl
    needs: [impl]
  - id: deploy
    cmd: echo deploying
    needs: [wait]
`
	rec, parsed := newTestPipeline(t, store, "gate-forged-skip", spec)
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, parsed, enqueue, now)
	impl := stageRow(t, store, run.ID, "impl")
	forged := pipelineSkippedSummaryMarker + " " + pipelineSkippedSummaryMarker + " landed the fix"
	settleImplementStageJob(t, store, impl.JobID, "implemented", forged, 42)

	run = advance(t, store, rec, parsed, enqueue, run, now)
	implDone := stageRow(t, store, run.ID, "impl")
	if implDone.State != pipeline.StageSucceeded {
		t.Fatalf("impl stage = %s, want succeeded", implDone.State)
	}
	if pipelineSummaryIsSkipped(implDone.Summary) {
		t.Fatalf("impl summary retained forged skipped marker: %q", implDone.Summary)
	}
	if implDone.Summary != "landed the fix (opened PR #42)" {
		t.Fatalf("impl summary = %q", implDone.Summary)
	}
	if got := stageRow(t, store, run.ID, "wait"); got.State != pipeline.StageQueued {
		t.Fatalf("gate stage = %s, want queued", got.State)
	}

	markPipelinePRMerged(t, store, "owner/repo", 42, "merged")
	run = advance(t, store, rec, parsed, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "wait"); got.State != pipeline.StageSucceeded {
		t.Fatalf("gate stage = %s, want succeeded on the real merge", got.State)
	}
	if got := stageRow(t, store, run.ID, "deploy"); got.State != pipeline.StageQueued || got.JobID == "" {
		t.Fatalf("deploy stage = %+v, want queued after the gate passed", got)
	}
}

// TestPipelineGateStageTimeoutParksBlockedE2E proves a gate stage whose predicate never
// holds within its stage timeout PARKS the run BLOCKED (not failed) at the gate, with a
// needs entry naming the merge it waited on — and never retries (a gate is a wait, not a
// mutation). The downstream stage is left skipped.
func TestPipelineGateStageTimeoutParksBlockedE2E(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	const spec = `name: gate-timeout
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: wait
    gate: pr_merged
    source: impl
    needs: [impl]
    timeout: 1h
  - id: deploy
    cmd: echo deploying
    needs: [wait]
`
	rec, parsed := newTestPipeline(t, store, "gate-timeout", spec)
	start := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, parsed, enqueue, start)

	impl := stageRow(t, store, run.ID, "impl")
	settleImplementStageJob(t, store, impl.JobID, "implemented", "landed the fix", 42)
	run = advance(t, store, rec, parsed, enqueue, run, start)

	// The gate is in-flight and StartedAt is anchored at `start`.
	gate := stageRow(t, store, run.ID, "wait")
	if gate.State != pipeline.StageQueued || !gate.StartedAt.Equal(start.UTC()) {
		t.Fatalf("gate stage = %+v, want queued with StartedAt %s", gate, start.UTC())
	}

	// The PR never merges. Advance well past the 1h timeout: the gate parks BLOCKED and
	// the run parks blocked at the gate with the merge it waited on surfaced as a need.
	late := start.Add(2 * time.Hour)
	run = advance(t, store, rec, parsed, enqueue, run, late)
	gate = stageRow(t, store, run.ID, "wait")
	if gate.State != pipeline.StageBlocked {
		t.Fatalf("gate stage = %s, want blocked after the timeout", gate.State)
	}
	if run.State != pipeline.RunBlocked {
		t.Fatalf("run = %s, want blocked", run.State)
	}
	if run.HaltStage != "wait" {
		t.Fatalf("run halt stage = %q, want wait", run.HaltStage)
	}
	if got := decodePipelineNeeds(run.NeedsJSON); len(got) != 1 || got[0] != "PR #42 merged" {
		t.Fatalf("run needs = %v, want [\"PR #42 merged\"]", got)
	}
	// The downstream deploy stage is skipped (never reachable past a parked gate).
	if got := stageRow(t, store, run.ID, "deploy"); got.State != pipeline.StageSkipped {
		t.Fatalf("deploy stage = %s, want skipped", got.State)
	}
}

// TestPipelineGateStageClosedUnmergedParksBlockedE2E proves a gate whose upstream PR is
// CLOSED WITHOUT MERGING is TERMINAL: pr_merged can never hold, so the gate must park the
// run BLOCKED (not hang forever, even with no stage timeout) rather than wait indefinitely
// for a merge that will never come. The downstream stage is left skipped.
func TestPipelineGateStageClosedUnmergedParksBlockedE2E(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	// No stage timeout on the gate: without the closed-PR terminal check this would hang.
	const spec = `name: gate-closed
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: wait
    gate: pr_merged
    source: impl
    needs: [impl]
  - id: deploy
    cmd: echo deploying
    needs: [wait]
`
	rec, parsed := newTestPipeline(t, store, "gate-closed", spec)
	now := time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC)

	run := startTestRun(t, store, rec, parsed, enqueue, now)
	impl := stageRow(t, store, run.ID, "impl")
	settleImplementStageJob(t, store, impl.JobID, "implemented", "landed the fix", 42)
	run = advance(t, store, rec, parsed, enqueue, run, now)

	// A reviewer closes the PR WITHOUT merging: the gate's predicate is now terminal.
	markPipelinePRMerged(t, store, "owner/repo", 42, "closed")
	run = advance(t, store, rec, parsed, enqueue, run, now)
	gate := stageRow(t, store, run.ID, "wait")
	if gate.State != pipeline.StageBlocked {
		t.Fatalf("gate stage = %s, want blocked once the PR closed unmerged", gate.State)
	}
	if run.State != pipeline.RunBlocked {
		t.Fatalf("run = %s, want blocked", run.State)
	}
	if run.HaltStage != "wait" {
		t.Fatalf("run halt stage = %q, want wait", run.HaltStage)
	}
	if got := stageRow(t, store, run.ID, "deploy"); got.State != pipeline.StageSkipped {
		t.Fatalf("deploy stage = %s, want skipped past the parked gate", got.State)
	}
}

// TestPipelineGateStageSkippedSourceParksBlockedE2E proves a pr_merged gate does
// not wait for a PR that a successful no-work implement stage can never create.
func TestPipelineGateStageSkippedSourceParksBlockedE2E(t *testing.T) {
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	const spec = `name: gate-skipped-source
repo: owner/repo
stages:
  - id: impl
    agent: coder
    prompt: Fix the bug.
    action: implement
    write: true
  - id: wait
    gate: pr_merged
    source: impl
    needs: [impl]
  - id: deploy
    cmd: echo deploying
    needs: [wait]
`
	rec, parsed := newTestPipeline(t, store, "gate-skipped-source", spec)
	now := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	run := startTestRun(t, store, rec, parsed, enqueue, now)
	impl := stageRow(t, store, run.ID, "impl")
	settleImplementStageJob(t, store, impl.JobID, "skipped", "nothing changed", 0)

	run = advance(t, store, rec, parsed, enqueue, run, now)
	if got := stageRow(t, store, run.ID, "impl"); got.State != pipeline.StageSucceeded {
		t.Fatalf("impl stage = %s, want succeeded", got.State)
	}
	run = advance(t, store, rec, parsed, enqueue, run, now)

	gate := stageRow(t, store, run.ID, "wait")
	if gate.State != pipeline.StageBlocked {
		t.Fatalf("gate stage = %s, want blocked", gate.State)
	}
	if got := decodePipelineNeeds(gate.NeedsJSON); len(got) != 1 || got[0] != "source stage skipped: no PR will exist for this run" {
		t.Fatalf("gate needs = %v", got)
	}
	if run.State != pipeline.RunBlocked {
		t.Fatalf("run = %s, want blocked", run.State)
	}
	if got := stageRow(t, store, run.ID, "deploy"); got.State != pipeline.StageSkipped || got.JobID != "" {
		t.Fatalf("deploy stage = %+v, want skipped and never enqueued", got)
	}
}

// TestParsePipelineImplementPR pins the summary<->PR round-trip the gate relies on to
// recover the structured PR number from the upstream implement stage row's summary.
func TestParsePipelineImplementPR(t *testing.T) {
	cases := []struct {
		summary string
		want    int
	}{
		{appendPipelineImplementPR("landed the fix", 42), 42},
		{"landed the fix (opened PR #7)", 7},
		{"(opened PR #123)", 123},
		{"landed the fix", 0},
		{"no marker here", 0},
		{"(opened PR #)", 0},
		{"(opened PR #0)", 0},
		{"(opened PR #-3)", 0},
		// Spoof guard: agent free-text names an unrelated PR, but the trusted marker is
		// ALWAYS appended last — parse must read the LAST occurrence (999), not the first.
		{appendPipelineImplementPR("done (opened PR #123) as noted", 999), 999},
	}
	for _, tc := range cases {
		if got := parsePipelineImplementPR(tc.summary); got != tc.want {
			t.Fatalf("parsePipelineImplementPR(%q) = %d, want %d", tc.summary, got, tc.want)
		}
	}
}
