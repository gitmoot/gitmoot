package pipeline

import (
	"context"
	"fmt"
	"github.com/gitmoot/gitmoot/internal/db"
	"strings"
	"testing"
	"time"
)

// newScheduledPipeline stores an enabled/disabled pipeline carrying an interval
// schedule. It writes the interval/jitter/enabled columns DIRECTLY — those DB
// columns (not the spec YAML) are what the scan-based scheduler reads, exactly as
// `pipeline add` maps spec.Schedule into them. next_due starts zero (== due), so a
// freshly scheduled pipeline is due on the first scan.
func newScheduledPipeline(t *testing.T, store *db.Store, name, repo, interval, jitter string, enabled bool) db.Pipeline {
	t.Helper()
	specYAML := "name: " + name + "\nstages:\n  - id: a\n    cmd: echo a\n  - id: b\n    cmd: echo b\n    needs: [a]\n"
	rec := db.Pipeline{
		Name:     name,
		Repo:     repo,
		SpecYAML: specYAML,
		SpecHash: Hash([]byte(specYAML)),
		Interval: interval,
		Jitter:   jitter,
		Enabled:  enabled,
	}
	if err := store.CreateOrUpdatePipeline(context.Background(), rec); err != nil {
		t.Fatalf("CreateOrUpdatePipeline: %v", err)
	}
	got, ok, err := store.GetPipeline(context.Background(), name)
	if err != nil || !ok {
		t.Fatalf("GetPipeline: ok=%v err=%v", ok, err)
	}
	return got
}

func newPipelineTriggeredPipeline(t *testing.T, store *db.Store, name, upstream string, enabled bool, armedAt time.Time) db.Pipeline {
	t.Helper()
	specYAML := fmt.Sprintf("name: %s\nrepo: owner/%s\ntrigger: {kind: pipeline, pipeline: %s}\nstages:\n  - {id: run, cmd: echo ok}\n  - {id: publish, cmd: echo publish, needs: [run]}\n", name, name, upstream)
	rec := db.Pipeline{Name: name, Repo: "owner/" + name, SpecYAML: specYAML, SpecHash: Hash([]byte(specYAML)), Enabled: enabled}
	if err := store.CreateOrUpdatePipeline(context.Background(), rec); err != nil {
		t.Fatalf("CreateOrUpdatePipeline: %v", err)
	}
	if err := store.ArmPipelineTrigger(context.Background(), name, upstream, armedAt); err != nil {
		t.Fatalf("ArmPipelineTrigger: %v", err)
	}
	got, ok, err := store.GetPipeline(context.Background(), name)
	if err != nil || !ok {
		t.Fatalf("GetPipeline: ok=%v err=%v", ok, err)
	}
	return got
}

func seedPipelineRunState(t *testing.T, store *db.Store, id, name, state string, started time.Time) db.PipelineRun {
	t.Helper()
	run := db.PipelineRun{ID: id, Pipeline: name, Trigger: "manual", State: state, StartedAt: started}
	if err := store.CreatePipelineRun(context.Background(), run); err != nil {
		t.Fatalf("CreatePipelineRun %s: %v", id, err)
	}
	return run
}

func TestPipelineSuccessTriggerFiresOnce(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	upstreamSpec := "name: upstream\nstages: [{id: run, cmd: echo}]\n"
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "upstream", SpecYAML: upstreamSpec, SpecHash: Hash([]byte(upstreamSpec))}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	newPipelineTriggeredPipeline(t, store, "downstream", "upstream", true, base)
	upstream := seedPipelineRunState(t, store, "prun-up-success", "upstream", RunSucceeded, base.Add(time.Minute))

	if err := TriggerPipelineRuns(ctx, store, base.Add(2*time.Minute)); err != nil {
		t.Fatalf("TriggerPipelineRuns: %v", err)
	}
	runs := pipelineRunCount(t, store, "downstream")
	if len(runs) != 1 || runs[0].Trigger != "pipeline" {
		t.Fatalf("downstream runs = %+v, want one pipeline-triggered run", runs)
	}
	if !strings.Contains(runs[0].PayloadJSON, upstream.ID) || !strings.Contains(runs[0].PayloadJSON, `"upstream_pipeline":"upstream"`) {
		t.Fatalf("downstream payload_json = %q", runs[0].PayloadJSON)
	}
	if got := stageRow(t, store, runs[0].ID, "run").NeedsJSON; got != "" {
		t.Fatalf("triggered root needs_json = %q, want empty", got)
	}
	if got := stageRow(t, store, runs[0].ID, "publish").NeedsJSON; got != `["run"]` {
		t.Fatalf("triggered publish needs_json = %q, want [run]", got)
	}
	if err := TriggerPipelineRuns(ctx, store, base.Add(3*time.Minute)); err != nil {
		t.Fatalf("re-tick: %v", err)
	}
	if got := len(pipelineRunCount(t, store, "downstream")); got != 1 {
		t.Fatalf("re-tick created %d downstream runs, want 1", got)
	}
	state, _, _ := store.GetPipelineTriggerState(ctx, "downstream")
	if state.Cursor != upstream.ID {
		t.Fatalf("cursor = %q, want %q", state.Cursor, upstream.ID)
	}
}

func TestPipelineSuccessTriggerIgnoresFailedCancelledAndDisabled(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	upstreamSpec := "name: upstream\nstages: [{id: run, cmd: echo}]\n"
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "upstream", SpecYAML: upstreamSpec, SpecHash: Hash([]byte(upstreamSpec))}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 14, 11, 0, 0, 0, time.UTC)
	newPipelineTriggeredPipeline(t, store, "downstream", "upstream", true, base)
	seedPipelineRunState(t, store, "prun-up-failed", "upstream", RunFailed, base.Add(time.Minute))
	seedPipelineRunState(t, store, "prun-up-cancelled", "upstream", RunCancelled, base.Add(2*time.Minute))
	if err := TriggerPipelineRuns(ctx, store, base.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := len(pipelineRunCount(t, store, "downstream")); got != 0 {
		t.Fatalf("failed/cancelled upstream created %d runs", got)
	}

	if err := store.SetPipelineEnabled(ctx, "downstream", false); err != nil {
		t.Fatal(err)
	}
	seedPipelineRunState(t, store, "prun-up-success", "upstream", RunSucceeded, base.Add(4*time.Minute))
	if err := TriggerPipelineRuns(ctx, store, base.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := len(pipelineRunCount(t, store, "downstream")); got != 0 {
		t.Fatalf("disabled downstream created %d runs", got)
	}
	state, _, _ := store.GetPipelineTriggerState(ctx, "downstream")
	if state.Cursor != "" {
		t.Fatalf("disabled downstream advanced cursor to %q", state.Cursor)
	}
}

func TestPipelineSuccessTriggerActiveRunDefersWithoutCursorAdvance(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	upstreamSpec := "name: upstream\nstages: [{id: run, cmd: echo}]\n"
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "upstream", SpecYAML: upstreamSpec, SpecHash: Hash([]byte(upstreamSpec))}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	rec := newPipelineTriggeredPipeline(t, store, "downstream", "upstream", true, base)
	upstream := seedPipelineRunState(t, store, "prun-up-success", "upstream", RunSucceeded, base.Add(time.Minute))
	spec, err := Load([]byte(rec.SpecYAML))
	if err != nil {
		t.Fatal(err)
	}
	active, err := CreatePipelineRun(ctx, store, rec, spec, "manual", "{}", base.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := TriggerPipelineRuns(ctx, store, base.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	state, _, _ := store.GetPipelineTriggerState(ctx, "downstream")
	if state.Cursor != "" || len(pipelineRunCount(t, store, "downstream")) != 1 {
		t.Fatalf("active guard state=%+v runs=%+v", state, pipelineRunCount(t, store, "downstream"))
	}
	active.State = RunSucceeded
	active.FinishedAt = base.Add(4 * time.Minute)
	if err := store.UpdatePipelineRun(ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := TriggerPipelineRuns(ctx, store, base.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	state, _, _ = store.GetPipelineTriggerState(ctx, "downstream")
	if state.Cursor != upstream.ID || len(pipelineRunCount(t, store, "downstream")) != 2 {
		t.Fatalf("deferred fire state=%+v runs=%+v", state, pipelineRunCount(t, store, "downstream"))
	}
}

func TestPipelineEnvironmentPreflightScheduleAdvancesAndTriggerRetries(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	base := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	scheduledYAML := "name: scheduled-env\nrepo: owner/repo\nstages: [{id: run, cmd: echo, env_keys: [MISSING]}]\n"
	scheduled := db.Pipeline{
		Name: "scheduled-env", Repo: "owner/repo", SpecYAML: scheduledYAML,
		SpecHash: Hash([]byte(scheduledYAML)), Interval: "1h", Enabled: true,
	}
	if err := store.CreateOrUpdatePipeline(ctx, scheduled); err != nil {
		t.Fatal(err)
	}
	scheduled, _, _ = store.GetPipeline(ctx, scheduled.Name)
	if err := scheduleOnePipeline(ctx, store, scheduled, base); err == nil || !strings.Contains(err.Error(), "gitmoot key grant MISSING --pipeline scheduled-env") {
		t.Fatalf("schedule preflight error=%v", err)
	}
	updated, _, err := store.GetPipeline(ctx, scheduled.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.NextDueAt.After(base) {
		t.Fatalf("schedule next_due=%v, want after %v", updated.NextDueAt, base)
	}
	if runs := pipelineRunCount(t, store, scheduled.Name); len(runs) != 0 {
		t.Fatalf("schedule created unusable runs: %+v", runs)
	}

	upstreamYAML := "name: upstream-env\nstages: [{id: run, cmd: echo}]\n"
	if err := store.CreateOrUpdatePipeline(ctx, db.Pipeline{Name: "upstream-env", SpecYAML: upstreamYAML, SpecHash: Hash([]byte(upstreamYAML))}); err != nil {
		t.Fatal(err)
	}
	triggeredYAML := "name: triggered-env\nrepo: owner/repo\ntrigger: {kind: pipeline, pipeline: upstream-env}\nstages: [{id: run, cmd: echo, env_keys: [MISSING]}]\n"
	triggered := db.Pipeline{Name: "triggered-env", Repo: "owner/repo", SpecYAML: triggeredYAML, SpecHash: Hash([]byte(triggeredYAML)), Enabled: true}
	if err := store.CreateOrUpdatePipeline(ctx, triggered); err != nil {
		t.Fatal(err)
	}
	if err := store.ArmPipelineTrigger(ctx, triggered.Name, "upstream-env", base); err != nil {
		t.Fatal(err)
	}
	seedPipelineRunState(t, store, "prun-upstream-env", "upstream-env", RunSucceeded, base.Add(time.Minute))
	triggered, _, _ = store.GetPipeline(ctx, triggered.Name)
	if err := triggerOnePipeline(ctx, store, triggered, base.Add(2*time.Minute)); err == nil || !strings.Contains(err.Error(), "gitmoot key grant MISSING --pipeline triggered-env") {
		t.Fatalf("trigger preflight error=%v", err)
	}
	state, found, err := store.GetPipelineTriggerState(ctx, triggered.Name)
	if err != nil || !found {
		t.Fatalf("trigger state found=%v err=%v", found, err)
	}
	if state.Cursor != "" {
		t.Fatalf("trigger cursor advanced to %q", state.Cursor)
	}
	if runs := pipelineRunCount(t, store, triggered.Name); len(runs) != 0 {
		t.Fatalf("trigger created unusable runs: %+v", runs)
	}
}

func pipelineRunCount(t *testing.T, store *db.Store, name string) []db.PipelineRun {
	t.Helper()
	runs, err := store.ListPipelineRuns(context.Background(), name)
	if err != nil {
		t.Fatalf("ListPipelineRuns: %v", err)
	}
	return runs
}

// TestSchedulePipelineDueFires proves an enabled, due (zero next_due) scheduled
// pipeline gets exactly one scheduled run and its next_due advances one interval
// past now (the heartbeat anchor idiom).
func TestSchedulePipelineDueFires(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	newScheduledPipeline(t, store, "nightly", "owner/repo", "24h", "", true)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	if err := schedulePipelineRuns(ctx, store, now); err != nil {
		t.Fatalf("schedulePipelineRuns: %v", err)
	}
	runs := pipelineRunCount(t, store, "nightly")
	if len(runs) != 1 {
		t.Fatalf("due schedule created %d runs, want 1", len(runs))
	}
	if runs[0].Trigger != "schedule" {
		t.Fatalf("run trigger = %q, want schedule", runs[0].Trigger)
	}
	if runs[0].State != RunRunning {
		t.Fatalf("run state = %q, want running", runs[0].State)
	}
	rec, _, _ := store.GetPipeline(ctx, "nightly")
	if want := now.Add(24 * time.Hour); !rec.NextDueAt.Equal(want) {
		t.Fatalf("next_due = %s, want %s (anchored to now + interval)", rec.NextDueAt, want)
	}
}

// TestSchedulePipelineNotDue proves a future next_due is skipped without creating a
// run or advancing.
func TestSchedulePipelineNotDue(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	newScheduledPipeline(t, store, "nightly", "owner/repo", "24h", "", true)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	// Push next_due into the future: not due yet.
	future := now.Add(2 * time.Hour)
	if err := store.AdvancePipelineNextDue(ctx, "nightly", future); err != nil {
		t.Fatalf("AdvancePipelineNextDue: %v", err)
	}

	if err := schedulePipelineRuns(ctx, store, now); err != nil {
		t.Fatalf("schedulePipelineRuns: %v", err)
	}
	if runs := pipelineRunCount(t, store, "nightly"); len(runs) != 0 {
		t.Fatalf("not-due schedule created %d runs, want 0", len(runs))
	}
	rec, _, _ := store.GetPipeline(ctx, "nightly")
	if !rec.NextDueAt.Equal(future) {
		t.Fatalf("next_due = %s, want unchanged %s", rec.NextDueAt, future)
	}
}

// TestSchedulePipelineDisabledOrManual proves a disabled pipeline, and an enabled
// pipeline with no interval, are both never fired by the scheduler.
func TestSchedulePipelineDisabledOrManual(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	newScheduledPipeline(t, store, "off", "owner/repo", "24h", "", false) // disabled
	newScheduledPipeline(t, store, "manual", "owner/repo", "", "", true)  // enabled, no interval
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	if err := schedulePipelineRuns(ctx, store, now); err != nil {
		t.Fatalf("schedulePipelineRuns: %v", err)
	}
	if runs := pipelineRunCount(t, store, "off"); len(runs) != 0 {
		t.Fatalf("disabled pipeline fired %d runs, want 0", len(runs))
	}
	if runs := pipelineRunCount(t, store, "manual"); len(runs) != 0 {
		t.Fatalf("manual (no interval) pipeline fired %d runs, want 0", len(runs))
	}
}

// TestSchedulePipelineOverlapGuard proves a scheduled pipeline with an active
// (running) run is skipped WITHOUT advancing next_due, so no second run is created
// and the next one fires as soon as the active one settles.
func TestSchedulePipelineOverlapGuard(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	rec := newScheduledPipeline(t, store, "nightly", "owner/repo", "24h", "", true)
	spec, err := Load([]byte(rec.SpecYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	// An already-running run exists (created earlier).
	if _, err := CreatePipelineRun(ctx, store, rec, spec, "schedule", "{}", now.Add(-time.Hour)); err != nil {
		t.Fatalf("CreatePipelineRun: %v", err)
	}

	if err := schedulePipelineRuns(ctx, store, now); err != nil {
		t.Fatalf("schedulePipelineRuns: %v", err)
	}
	if runs := pipelineRunCount(t, store, "nightly"); len(runs) != 1 {
		t.Fatalf("overlap guard created a second run: %d runs, want 1", len(runs))
	}
	got, _, _ := store.GetPipeline(ctx, "nightly")
	if !got.NextDueAt.IsZero() {
		t.Fatalf("overlap guard advanced next_due to %s, want unchanged (zero)", got.NextDueAt)
	}
}

// TestSchedulePipelineCoalescesMissedTicks proves a long-idle scheduler (next_due
// far in the past) fires exactly ONE run and re-anchors next_due to now+interval —
// no backlog replay of every missed interval.
func TestSchedulePipelineCoalescesMissedTicks(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	newScheduledPipeline(t, store, "nightly", "owner/repo", "1h", "", true)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	// 100 intervals overdue (the daemon was down for days).
	if err := store.AdvancePipelineNextDue(ctx, "nightly", now.Add(-100*time.Hour)); err != nil {
		t.Fatalf("AdvancePipelineNextDue: %v", err)
	}

	if err := schedulePipelineRuns(ctx, store, now); err != nil {
		t.Fatalf("schedulePipelineRuns: %v", err)
	}
	if runs := pipelineRunCount(t, store, "nightly"); len(runs) != 1 {
		t.Fatalf("coalescing created %d runs, want exactly 1", len(runs))
	}
	rec, _, _ := store.GetPipeline(ctx, "nightly")
	if want := now.Add(time.Hour); !rec.NextDueAt.Equal(want) {
		t.Fatalf("next_due = %s, want %s (re-anchored to now, not replaying backlog)", rec.NextDueAt, want)
	}
}

// TestSchedulePipelineNoRepoAdvances proves a scheduled pipeline WITHOUT a repo is
// skipped (its stage jobs would wedge with no worker to claim them) but its next_due
// is advanced so a misconfigured schedule does not hot-loop.
func TestSchedulePipelineNoRepoAdvances(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	newScheduledPipeline(t, store, "norepo", "", "24h", "", true)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	if err := schedulePipelineRuns(ctx, store, now); err != nil {
		t.Fatalf("schedulePipelineRuns: %v", err)
	}
	if runs := pipelineRunCount(t, store, "norepo"); len(runs) != 0 {
		t.Fatalf("no-repo schedule created %d runs, want 0", len(runs))
	}
	rec, _, _ := store.GetPipeline(ctx, "norepo")
	if want := now.Add(24 * time.Hour); !rec.NextDueAt.Equal(want) {
		t.Fatalf("next_due = %s, want %s (advanced to avoid hot-loop)", rec.NextDueAt, want)
	}
}

// TestSchedulePipelineJitterWithinBounds proves next_due lands in
// [now+interval, now+interval+jitter] when a jitter is set.
func TestSchedulePipelineJitterWithinBounds(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	newScheduledPipeline(t, store, "jittered", "owner/repo", "1h", "15m", true)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	if err := schedulePipelineRuns(ctx, store, now); err != nil {
		t.Fatalf("schedulePipelineRuns: %v", err)
	}
	rec, _, _ := store.GetPipeline(ctx, "jittered")
	lo := now.Add(time.Hour)
	hi := now.Add(time.Hour + 15*time.Minute)
	if rec.NextDueAt.Before(lo) || rec.NextDueAt.After(hi) {
		t.Fatalf("next_due = %s, want within [%s, %s]", rec.NextDueAt, lo, hi)
	}
}

// TestRunPipelineScanOnceScheduleThenAdvance proves the full scan wires the two
// passes in order: the schedule pass creates the due run, and the advance pass that
// follows in the SAME scan enqueues its ready root stage.
func TestRunPipelineScanOnceScheduleThenAdvance(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)
	enqueue := testStageEnqueuer(store)
	newScheduledPipeline(t, store, "nightly", "owner/repo", "24h", "", true)
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	if err := RunPipelineScanOnce(ctx, store, enqueue, now); err != nil {
		t.Fatalf("RunPipelineScanOnce: %v", err)
	}
	runs := pipelineRunCount(t, store, "nightly")
	if len(runs) != 1 {
		t.Fatalf("scan created %d runs, want 1", len(runs))
	}
	// The advance pass in the same scan enqueued the root stage (a), leaving b pending.
	a := stageRow(t, store, runs[0].ID, "a")
	if a.State != StageQueued || a.JobID == "" {
		t.Fatalf("root stage a = %+v, want queued with a job (advance pass ran)", a)
	}
	if b := stageRow(t, store, runs[0].ID, "b"); b.State != StagePending {
		t.Fatalf("stage b = %s, want pending", b.State)
	}
}
