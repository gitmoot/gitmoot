package db

import (
	"context"
	"path/filepath"
	"testing"
)

var _ func(*Store, context.Context, string, string, int64, string, string, JobEvent, ...JobEvent) (bool, error) = (*Store).TransitionJobStatePayloadWithEventAtGeneration

func openLifecycleGenerationStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gitmoot.db"))
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
