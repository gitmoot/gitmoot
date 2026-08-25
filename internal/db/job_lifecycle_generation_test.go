package db

import (
	"context"
	"path/filepath"
	"testing"
)

var _ func(*Store, context.Context, string, string, int64, string, string, JobEvent, ...JobEvent) (bool, error) = (*Store).TransitionJobStatePayloadWithEventAtGeneration

// The two payload writers must keep DIFFERENT shapes: one anchored, one not. Pinning both
// signatures makes "collapse them into one function" a COMPILE failure rather than a silent
// behaviour change, which is the failure mode TestUpdateJobPayloadStaysUnconditional guards
// at runtime.
var _ func(*Store, context.Context, string, string) error = (*Store).UpdateJobPayload

var _ func(*Store, context.Context, string, string, int64) (bool, error) = (*Store).UpdateJobPayloadAtGeneration

func openLifecycleGenerationStore(t *testing.T) *Store {
	t.Helper()
	store, err := openCachedTestStore(t, filepath.Join(t.TempDir(), "gitmoot.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestLifecycleGenerationBumpsOnlyOnEntryToQueued pins the PREMISE the whole ABA fix rests
// on (#1407): that jobs.lifecycle_generation advances exactly when a write moves a job INTO
// queued from another state, and at no other time.
//
// It is a db-level test rather than a cli-level one on purpose. The counter is maintained in
// the SET clause of the state-writing UPDATEs, so its correctness depends on a SQLite
// evaluation rule -- that expressions in a SET clause read the row's PRE-UPDATE values, so
// `state <> 'queued'` sees the state being LEFT rather than the one being written. That rule
// is assumed by every consumer and verified nowhere else; if it were the other way round the
// bump would never fire and every anchored CAS above would silently degrade to the state-only
// behaviour this fix exists to remove, with all the consumer tests still green.
func TestLifecycleGenerationBumpsOnlyOnEntryToQueued(t *testing.T) {
	ctx := context.Background()
	store := openLifecycleGenerationStore(t)

	if err := store.CreateJob(ctx, Job{ID: "gen", Agent: "lead", Type: "implement", State: "queued", Payload: "{}"}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	generation := func() int64 {
		t.Helper()
		job, err := store.GetJob(ctx, "gen")
		if err != nil {
			t.Fatalf("GetJob returned error: %v", err)
		}
		return job.LifecycleGeneration
	}

	if got := generation(); got != 0 {
		t.Fatalf("generation at creation = %d, want 0", got)
	}

	// queued -> running -> failed: no entry to queued, so no bump. Without this arm a
	// mutant that bumps on EVERY state write would still pass the entry-to-queued arm
	// below, and the counter would advance under a live advancement that never retried.
	if _, err := store.TransitionJobState(ctx, "gen", "queued", "running"); err != nil {
		t.Fatalf("TransitionJobState(queued->running) returned error: %v", err)
	}
	if got := generation(); got != 0 {
		t.Fatalf("generation after queued->running = %d, want 0 (only entry to queued is a new run)", got)
	}
	if _, err := store.TransitionJobState(ctx, "gen", "running", "failed"); err != nil {
		t.Fatalf("TransitionJobState(running->failed) returned error: %v", err)
	}
	if got := generation(); got != 0 {
		t.Fatalf("generation after running->failed = %d, want 0", got)
	}

	// failed -> queued: a retry. This is the bump, and it is the one the anchored CAS reads.
	if _, err := store.TransitionJobState(ctx, "gen", "failed", "queued"); err != nil {
		t.Fatalf("TransitionJobState(failed->queued) returned error: %v", err)
	}
	if got := generation(); got != 1 {
		t.Fatalf("generation after failed->queued = %d, want 1 (a retry is a new run)", got)
	}

	// queued -> queued is not a new run. UpdateJobState writes state unconditionally, so
	// without the `state <> 'queued'` term any re-write of an already-queued job would
	// invalidate a live advancement's anchor for no reason.
	if err := store.UpdateJobState(ctx, "gen", "queued"); err != nil {
		t.Fatalf("UpdateJobState(queued) returned error: %v", err)
	}
	if got := generation(); got != 1 {
		t.Fatalf("generation after queued->queued = %d, want 1 (a re-write of the same state is not a new run)", got)
	}

	// A second full lifecycle bumps again: the counter is monotonic, which is the property
	// that makes it immune to the returning-value problem a state string has.
	if _, err := store.TransitionJobState(ctx, "gen", "queued", "running"); err != nil {
		t.Fatalf("TransitionJobState(queued->running) returned error: %v", err)
	}
	if _, err := store.TransitionJobState(ctx, "gen", "running", "failed"); err != nil {
		t.Fatalf("TransitionJobState(running->failed) returned error: %v", err)
	}
	if _, err := store.TransitionJobState(ctx, "gen", "failed", "queued"); err != nil {
		t.Fatalf("TransitionJobState(failed->queued) returned error: %v", err)
	}
	if got := generation(); got != 2 {
		t.Fatalf("generation after a second retry = %d, want 2", got)
	}
}

// TestTransitionJobStateWithEventAtGenerationRefusesStaleGeneration pins the CAS itself: a
// caller holding the right state but a stale generation must lose, and must not write its
// event.
//
// The state arm and the generation arm are asserted separately. A single case where both are
// stale cannot tell an anchored CAS from the plain state-only one -- both refuse it -- so the
// discriminating case is the one where the STATE MATCHES and only the generation has moved.
// That is exactly the ABA shape, and it is the only case that distinguishes this function
// from TransitionJobStateWithEvent.
func TestTransitionJobStateWithEventAtGenerationRefusesStaleGeneration(t *testing.T) {
	ctx := context.Background()
	store := openLifecycleGenerationStore(t)
	if err := store.CreateJob(ctx, Job{ID: "cas", Agent: "lead", Type: "implement", State: "queued", Payload: "{}"}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}

	// Drive one full lifecycle so the job is back at its ORIGINAL state string with a
	// generation that has moved: queued -> running -> failed -> queued -> running -> failed.
	for _, step := range [][2]string{
		{"queued", "running"}, {"running", "failed"}, {"failed", "queued"},
		{"queued", "running"}, {"running", "failed"},
	} {
		if _, err := store.TransitionJobState(ctx, "cas", step[0], step[1]); err != nil {
			t.Fatalf("TransitionJobState(%s->%s) returned error: %v", step[0], step[1], err)
		}
	}
	job, err := store.GetJob(ctx, "cas")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != "failed" || job.LifecycleGeneration != 1 {
		t.Fatalf("fixture = state %q generation %d, want failed/1 -- the ABA setup did not arm", job.State, job.LifecycleGeneration)
	}

	// The stale caller observed "failed" at generation 0. The state MATCHES the row.
	transitioned, err := store.TransitionJobStateWithEventAtGeneration(ctx, "cas", "failed", 0, "blocked", JobEvent{
		JobID: "cas", Kind: "advance_blocked", Message: "stale settlement",
	})
	if err != nil {
		t.Fatalf("TransitionJobStateWithEventAtGeneration returned error: %v", err)
	}
	if transitioned {
		t.Fatal("a settlement anchored to generation 0 won the CAS against generation 1: the anchor is not being applied (this is the ABA the fix exists to stop)")
	}
	after, err := store.GetJob(ctx, "cas")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if after.State != "failed" {
		t.Fatalf("state after refused CAS = %q, want failed (unchanged)", after.State)
	}
	jobEvents, err := store.ListJobEvents(ctx, "cas")
	if err != nil {
		t.Fatalf("ListJobEvents returned error: %v", err)
	}
	for _, event := range jobEvents {
		if event.Kind == "advance_blocked" {
			t.Fatal("refused CAS still wrote its event; the event must be transactional with the transition")
		}
	}

	// Control: the SAME call at the CURRENT generation succeeds. Without it, a mutant
	// returning false unconditionally would pass every assertion above.
	transitioned, err = store.TransitionJobStateWithEventAtGeneration(ctx, "cas", "failed", 1, "blocked", JobEvent{
		JobID: "cas", Kind: "advance_blocked", Message: "live settlement",
	})
	if err != nil {
		t.Fatalf("TransitionJobStateWithEventAtGeneration returned error: %v", err)
	}
	if !transitioned {
		t.Fatal("a settlement anchored to the CURRENT generation lost the CAS; the anchor rejects live callers too")
	}
	live, err := store.GetJob(ctx, "cas")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if live.State != "blocked" {
		t.Fatalf("state after accepted CAS = %q, want blocked", live.State)
	}
}

func TestTransitionJobStatePayloadWithEventAtGenerationAnchorsPayload(t *testing.T) {
	ctx := context.Background()
	store := openLifecycleGenerationStore(t)
	if err := store.CreateJob(ctx, Job{ID: "payload-cas", Agent: "lead", Type: "implement", State: "queued", Payload: `{"result":{"decision":"implemented"}}`}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	for _, step := range [][2]string{
		{"queued", "running"}, {"running", "failed"}, {"failed", "queued"},
		{"queued", "running"}, {"running", "failed"},
	} {
		if _, err := store.TransitionJobState(ctx, "payload-cas", step[0], step[1]); err != nil {
			t.Fatalf("TransitionJobState(%s->%s) returned error: %v", step[0], step[1], err)
		}
	}

	stalePayload := `{"result":{"decision":"blocked","summary":"stale"}}`
	transitioned, err := store.TransitionJobStatePayloadWithEventAtGeneration(ctx, "payload-cas", "failed", 0, "blocked", stalePayload, JobEvent{Kind: "advance_blocked", Message: "stale"})
	if err != nil {
		t.Fatalf("stale transition returned error: %v", err)
	}
	if transitioned {
		t.Fatal("stale generation replaced the live payload")
	}
	afterStale, err := store.GetJob(ctx, "payload-cas")
	if err != nil {
		t.Fatalf("GetJob after stale transition: %v", err)
	}
	if afterStale.Payload == stalePayload {
		t.Fatal("stale generation wrote its payload despite losing the CAS")
	}

	livePayload := `{"result":{"decision":"blocked","summary":"live"}}`
	transitioned, err = store.TransitionJobStatePayloadWithEventAtGeneration(ctx, "payload-cas", "failed", 1, "blocked", livePayload, JobEvent{Kind: "advance_blocked", Message: "live"})
	if err != nil {
		t.Fatalf("live transition returned error: %v", err)
	}
	if !transitioned {
		t.Fatal("current generation did not settle")
	}
	afterLive, err := store.GetJob(ctx, "payload-cas")
	if err != nil {
		t.Fatalf("GetJob after live transition: %v", err)
	}
	if afterLive.State != "blocked" || afterLive.Payload != livePayload {
		t.Fatalf("live settlement = state %q payload %q, want blocked and replacement payload", afterLive.State, afterLive.Payload)
	}
}

// armStaleLifecycleGeneration drives a freshly created queued job through one full retry so it
// lands at state "failed" with lifecycle_generation 1. That is the ABA shape the anchor exists
// for: a writer holding generation 0 reads back exactly the state string it expects, and the
// counter is the only thing that says it is stale.
func armStaleLifecycleGeneration(t *testing.T, store *Store, id string) {
	t.Helper()
	ctx := context.Background()
	for _, step := range [][2]string{
		{"queued", "running"}, {"running", "failed"}, {"failed", "queued"},
		{"queued", "running"}, {"running", "failed"},
	} {
		if _, err := store.TransitionJobState(ctx, id, step[0], step[1]); err != nil {
			t.Fatalf("TransitionJobState(%s->%s) returned error: %v", step[0], step[1], err)
		}
	}
	job, err := store.GetJob(ctx, id)
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if job.State != "failed" || job.LifecycleGeneration != 1 {
		t.Fatalf("fixture = state %q generation %d, want failed/1 -- the stale-generation setup did not arm", job.State, job.LifecycleGeneration)
	}
}

// TestUpdateJobPayloadAtGenerationRefusesStaleGeneration pins the payload-only CAS (#1620) at
// the layer that implements it.
//
// The discriminating case is a write whose anchor has gone stale while the row's STATE is
// unchanged: state-based guards cannot see it, so it is the only case that separates this
// function from UpdateJobPayload. Losing must be a SILENT NO-OP -- (false, nil), not an error --
// because the payload describes a superseded run and writing nothing is the correct outcome;
// returning an error would push callers into a retry that can only make the overwrite happen.
// The live payload is compared byte-for-byte rather than by decision field, so a partial write
// that lands the diagnostics but not the rest cannot pass.
func TestUpdateJobPayloadAtGenerationRefusesStaleGeneration(t *testing.T) {
	ctx := context.Background()
	store := openLifecycleGenerationStore(t)

	runTwoPayload := `{"result":{"decision":"implemented","summary":"run two"}}`
	if err := store.CreateJob(ctx, Job{ID: "payload-gen", Agent: "lead", Type: "implement", State: "queued", Payload: runTwoPayload}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}
	armStaleLifecycleGeneration(t, store, "payload-gen")

	// The stale writer observed generation 0 -- the run that has already been retried away.
	runOnePayload := `{"result":{"decision":"blocked","summary":"run one delivery diagnostics"}}`
	written, err := store.UpdateJobPayloadAtGeneration(ctx, "payload-gen", runOnePayload, 0)
	if err != nil {
		t.Fatalf("UpdateJobPayloadAtGeneration at a stale generation returned error %v; losing the CAS is a normal outcome and must not be reported as a failure", err)
	}
	if written {
		t.Fatal("a payload write anchored to generation 0 won against generation 1: the generation is not being applied in the WHERE clause, so run one's payload overwrites run two's (the #1620 race)")
	}

	afterStale, err := store.GetJob(ctx, "payload-gen")
	if err != nil {
		t.Fatalf("GetJob after the refused write returned error: %v", err)
	}
	if afterStale.Payload != runTwoPayload {
		t.Fatalf("payload after the refused write = %q, want %q byte-identical: a lost CAS must leave the row untouched", afterStale.Payload, runTwoPayload)
	}
	if afterStale.State != "failed" || afterStale.LifecycleGeneration != 1 {
		t.Fatalf("row after the refused write = state %q generation %d, want failed/1 unchanged", afterStale.State, afterStale.LifecycleGeneration)
	}

	// Control: the SAME call at the CURRENT generation lands. Without it a mutant that returns
	// (false, nil) unconditionally -- or one that never writes at all -- passes everything above.
	runTwoDiagnostics := `{"result":{"decision":"implemented","summary":"run two delivery diagnostics"}}`
	written, err = store.UpdateJobPayloadAtGeneration(ctx, "payload-gen", runTwoDiagnostics, 1)
	if err != nil {
		t.Fatalf("UpdateJobPayloadAtGeneration at the current generation returned error: %v", err)
	}
	if !written {
		t.Fatal("a payload write anchored to the CURRENT generation lost the CAS; the anchor is rejecting live callers too")
	}
	afterLive, err := store.GetJob(ctx, "payload-gen")
	if err != nil {
		t.Fatalf("GetJob after the accepted write returned error: %v", err)
	}
	if afterLive.Payload != runTwoDiagnostics {
		t.Fatalf("payload after the accepted write = %q, want %q", afterLive.Payload, runTwoDiagnostics)
	}
	// A payload write is not a lifecycle event. If it advanced the counter (or the state), every
	// anchor held by a concurrent settler would be invalidated by a plain result write, which is
	// a second way to lose the same data the CAS is here to protect.
	if afterLive.State != "failed" || afterLive.LifecycleGeneration != 1 {
		t.Fatalf("row after the accepted write = state %q generation %d, want failed/1: a payload write must advance neither", afterLive.State, afterLive.LifecycleGeneration)
	}
}

// TestUpdateJobPayloadStaysUnconditional pins that the UNTOUCHED UpdateJobPayload still writes
// at any generation.
//
// #1620 anchored one caller and deliberately left this function alone: its callers write
// payloads that were never anchored to an observed run, so they hold no generation to pass. The
// regression this guards is a later refactor collapsing the two -- routing the generation-less
// callers through the CAS behind some default anchor. That failure is SILENT by construction:
// the CAS reports a loss as (false, nil), so every such write would be dropped with no error
// raised anywhere, and only a job that had been retried at least once would lose its payload.
func TestUpdateJobPayloadStaysUnconditional(t *testing.T) {
	ctx := context.Background()
	store := openLifecycleGenerationStore(t)
	if err := store.CreateJob(ctx, Job{ID: "payload-plain", Agent: "lead", Type: "implement", State: "queued", Payload: `{"result":{"decision":"implemented"}}`}); err != nil {
		t.Fatalf("CreateJob returned error: %v", err)
	}

	// Generation 0 first: the value a mistakenly-anchored refactor would most plausibly default
	// to, and therefore the one arm that would keep passing after such a change.
	atZero := `{"result":{"decision":"implemented","summary":"written at generation 0"}}`
	if err := store.UpdateJobPayload(ctx, "payload-plain", atZero); err != nil {
		t.Fatalf("UpdateJobPayload at generation 0 returned error: %v", err)
	}
	beforeRetry, err := store.GetJob(ctx, "payload-plain")
	if err != nil {
		t.Fatalf("GetJob before the retry returned error: %v", err)
	}
	if beforeRetry.Payload != atZero {
		t.Fatalf("payload at generation 0 = %q, want %q", beforeRetry.Payload, atZero)
	}
	if beforeRetry.LifecycleGeneration != 0 {
		t.Fatalf("generation after an unconditional payload write = %d, want 0: a payload write is not a lifecycle event", beforeRetry.LifecycleGeneration)
	}

	armStaleLifecycleGeneration(t, store, "payload-plain")

	// Generation 1: the arm that actually discriminates. An anchored variant defaulting to 0
	// silently drops this write and reports nothing.
	atOne := `{"result":{"decision":"blocked","summary":"written at generation 1"}}`
	if err := store.UpdateJobPayload(ctx, "payload-plain", atOne); err != nil {
		t.Fatalf("UpdateJobPayload at generation 1 returned error: %v", err)
	}
	afterRetry, err := store.GetJob(ctx, "payload-plain")
	if err != nil {
		t.Fatalf("GetJob after the retry returned error: %v", err)
	}
	if afterRetry.Payload != atOne {
		t.Fatalf("payload after an unconditional write at generation 1 = %q, want %q: UpdateJobPayload has acquired a generation predicate its callers cannot satisfy", afterRetry.Payload, atOne)
	}
	if afterRetry.State != "failed" || afterRetry.LifecycleGeneration != 1 {
		t.Fatalf("row after an unconditional write = state %q generation %d, want failed/1: a payload write must advance neither", afterRetry.State, afterRetry.LifecycleGeneration)
	}
}
