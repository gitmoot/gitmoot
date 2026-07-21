package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestPipelineAdvanceWaitCapsUnderBackoff proves the #697 fix: when a pipeline
// run is in flight and the repo poller has backed off to a minutes-long wait, the
// supervisor sleep is capped to the (non-backed-off) poll interval so the pipeline
// advancer keeps ticking at its own cadence instead of the backoff cadence.
func TestPipelineAdvanceWaitCapsUnderBackoff(t *testing.T) {
	const pollFloor = 30 * time.Second
	backoff := 5 * time.Minute // pollRegisteredReposWithPoller's max backoff wait

	got := PipelineAdvanceWait(backoff, pollFloor, true)
	if got != pollFloor {
		t.Fatalf("PipelineAdvanceWait(%s, %s, inFlight) = %s, want the poll floor %s (advancer must not inherit the poll backoff)", backoff, pollFloor, got, pollFloor)
	}
}

// TestPipelineAdvanceWaitIdleUnchanged proves the UNCHANGED path: with no run in
// flight the backed-off poll wait is returned verbatim, so an idle daemon still
// sleeps out the full backoff exactly as before the fix (byte-identical cadence).
func TestPipelineAdvanceWaitIdleUnchanged(t *testing.T) {
	backoff := 5 * time.Minute
	if got := PipelineAdvanceWait(backoff, 30*time.Second, false); got != backoff {
		t.Fatalf("PipelineAdvanceWait(idle) = %s, want the poll wait unchanged %s", got, backoff)
	}
}

// TestPipelineAdvanceWaitNeverExtends proves the cap only ever SHORTENS the sleep:
// a normal (non-backed-off) poll wait shorter than the floor is returned as-is,
// even with a run in flight, so normal-cadence advancement is unaffected. It also
// covers a non-positive floor (misconfigured poll interval) leaving the wait alone.
func TestPipelineAdvanceWaitNeverExtends(t *testing.T) {
	if got := PipelineAdvanceWait(10*time.Second, 30*time.Second, true); got != 10*time.Second {
		t.Fatalf("PipelineAdvanceWait(shortWait, inFlight) = %s, want the shorter wait 10s (never extend)", got)
	}
	if got := PipelineAdvanceWait(2*time.Minute, 0, true); got != 2*time.Minute {
		t.Fatalf("PipelineAdvanceWait(zeroFloor) = %s, want the wait unchanged 2m", got)
	}
}

// TestPipelineRunsInFlight proves the in-flight predicate the supervisor uses to
// decide whether to cap the sleep: false with no pipelines, true while a run is
// running, and false again once the run settles.
func TestPipelineRunsInFlight(t *testing.T) {
	ctx := context.Background()
	store := pipelineAdvanceStore(t)

	if inFlight, err := PipelineRunsInFlight(ctx, store); err != nil || inFlight {
		t.Fatalf("PipelineRunsInFlight(empty) = %v err=%v, want false/nil", inFlight, err)
	}

	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)
	run := db.PipelineRun{ID: "prun-x", Pipeline: "p", State: RunRunning, StartedAt: now}
	if err := store.CreatePipelineRun(ctx, run); err != nil {
		t.Fatalf("CreatePipelineRun: %v", err)
	}
	if inFlight, err := PipelineRunsInFlight(ctx, store); err != nil || !inFlight {
		t.Fatalf("PipelineRunsInFlight(running) = %v err=%v, want true/nil", inFlight, err)
	}

	run.State = RunSucceeded
	run.FinishedAt = now.Add(time.Minute)
	if err := store.UpdatePipelineRun(ctx, run); err != nil {
		t.Fatalf("UpdatePipelineRun: %v", err)
	}
	if inFlight, err := PipelineRunsInFlight(ctx, store); err != nil || inFlight {
		t.Fatalf("PipelineRunsInFlight(settled) = %v err=%v, want false/nil", inFlight, err)
	}
}
