package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// A read-only seat's runtime state dir is built EMPTY by
// prepareReadOnlyRuntimeState (RemoveAll + MkdirAll, credential file only), so
// the isolated home cannot contain the session a concrete ref names. Measured
// failure: codex review job local-review-g7-review-18d1b8d23a6061a8 reached
// running and died with "thread/resume: no rollout found for thread id
// 019fa4c8-69c1-7bc2-8628-00ade8fa43c5" while that rollout sat in the real
// ~/.codex/sessions tree the seat is sandboxed away from.
func TestApplyReadOnlySeatRunsOnFreshSessionNotTheAgentsOwn(t *testing.T) {
	const storedRef = "019fa4c8-69c1-7bc2-8628-00ade8fa43c5"
	agent := runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeRef: storedRef}

	if err := applyReadOnlySeat(true, "/profiles/reviewer", "local-review-g7-review-18d1b8d2", &agent); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}

	if agent.RuntimeRef == storedRef {
		t.Fatalf("read-only seat kept the agent's resumable ref %q; the isolated home holds no session for it", storedRef)
	}
	if !runtime.IsFreshRef(agent.RuntimeRef) {
		t.Fatalf("read-only seat ref = %q, want a fresh: ref so the runtime starts a new session instead of resuming", agent.RuntimeRef)
	}
	if want := runtime.FreshRefForJob("local-review-g7-review-18d1b8d2"); agent.RuntimeRef != want {
		t.Fatalf("read-only seat ref = %q, want the job-scoped %q", agent.RuntimeRef, want)
	}
}

// A seat that kept the stored ref would also take the runtime-session lock on
// the agent's LIVE default-runtime session key: the lock is acquired on this
// effective agent (acquireJobRuntimeSessionLock), so a disposable report-only
// review would occupy the session a real job needs.
func TestApplyReadOnlySeatDoesNotOccupyTheAgentsSessionLockKey(t *testing.T) {
	stored := runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeRef: "019fa4c8-69c1-7bc2-8628-00ade8fa43c5"}
	storedKey, ok := runtimeSessionResourceKey(stored)
	if !ok {
		t.Fatal("stored codex session must have a lock key")
	}

	seat := stored
	if err := applyReadOnlySeat(true, "", "job-seat-1", &seat); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}
	seatKey, ok := runtimeSessionResourceKey(seat)
	if !ok {
		t.Fatal("read-only seat must still take a lock key")
	}
	if seatKey == storedKey {
		t.Fatalf("read-only seat locks the agent's own session key %q", seatKey)
	}

	other := stored
	if err := applyReadOnlySeat(true, "", "job-seat-2", &other); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}
	otherKey, _ := runtimeSessionResourceKey(other)
	if otherKey == seatKey {
		t.Fatalf("two read-only seats of one agent must not share a lock key, both = %q", seatKey)
	}
}

// An already-fresh ref is a per-job runtime override's minted ref or a
// registered fresh:<seat> ref scoped by scopeRegisteredFreshRefForJob. Both are
// unique per job AND are the keys the scheduler gate computed, so rewriting
// them here would desync gate from acquisition.
func TestApplyReadOnlySeatKeepsAnAlreadyFreshRef(t *testing.T) {
	minted, err := runtime.NewFreshRef()
	if err != nil {
		t.Fatalf("NewFreshRef: %v", err)
	}
	agent := runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeRef: minted}
	if err := applyReadOnlySeat(true, "", "job-override", &agent); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}
	if agent.RuntimeRef != minted {
		t.Fatalf("override ref rewritten to %q, want the enqueue-minted %q", agent.RuntimeRef, minted)
	}
}

// An ordinary (non-seat) job keeps its resumable session: this fix must not
// silently convert every job into a fresh session.
func TestApplyReadOnlySeatLeavesOrdinaryJobsResumable(t *testing.T) {
	agent := runtime.Agent{Runtime: runtime.CodexRuntime, RuntimeRef: "019fa4c8-69c1-7bc2-8628-00ade8fa43c5"}
	if err := applyReadOnlySeat(false, "/profiles/reviewer", "job-ordinary", &agent); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}
	if agent.RuntimeRef != "019fa4c8-69c1-7bc2-8628-00ade8fa43c5" || agent.ReadOnlySeat {
		t.Fatalf("ordinary job mutated: ref=%q seat=%v", agent.RuntimeRef, agent.ReadOnlySeat)
	}
}

// A shell ref is a COMMAND, not a resumable session, and isolated shell
// pipeline stages run with ReadOnlySeat=true. Rewriting that ref to a fresh
// session ref would replace the stage's work with nothing, which is exactly
// what the first version of this fix did to every shell pipeline E2E.
func TestApplyReadOnlySeatKeepsShellCommandRef(t *testing.T) {
	const command = `printf '%s' '{"gitmoot_result":{}}'`
	agent := runtime.Agent{Runtime: runtime.ShellRuntime, RuntimeRef: command}
	if err := applyReadOnlySeat(true, "", "pipe-run-stage-0", &agent); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}
	if agent.RuntimeRef != command {
		t.Fatalf("shell command ref rewritten to %q, want %q", agent.RuntimeRef, command)
	}
	if !agent.ReadOnlySeat {
		t.Fatal("shell stage lost its read-only seat marker")
	}
}

// The daemon SELECTOR must gate on exactly the key the worker ACQUIRES. A
// mismatch is the #1034 shape: the gate serializes read-only seats behind the
// agent's live session (or lets them through on a key nothing locks).
func TestQueuedJobRuntimeResourceKeyReadOnlySeat(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	stored := db.Agent{
		Name:       "gm-review-codex",
		Runtime:    runtime.CodexRuntime,
		RuntimeRef: "019fa4c8-69c1-7bc2-8628-00ade8fa43c5",
	}
	if err := store.UpsertAgent(ctx, stored); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	newJob := func(id string, seat bool) db.Job {
		encoded, err := json.Marshal(workflow.JobPayload{
			Repo:             "gitmoot/gitmoot",
			ReadOnlySeat:     seat,
			ReadOnlyWorktree: seat,
			WorktreePath:     "/wt/" + id,
		})
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return db.Job{ID: id, Agent: stored.Name, Payload: string(encoded)}
	}

	seatJob := newJob("local-review-seat-a", true)
	gate := queuedJobRuntimeResourceKey(ctx, store, seatJob)

	effective := runtimeAgent(stored)
	if err := applyReadOnlySeat(true, "", seatJob.ID, &effective); err != nil {
		t.Fatalf("applyReadOnlySeat: %v", err)
	}
	lockKey, ok := runtimeSessionResourceKey(effective)
	if !ok {
		t.Fatal("read-only seat must have an acquisition key")
	}
	if gate != lockKey {
		t.Fatalf("selector key %q must equal the acquisition key %q", gate, lockKey)
	}

	storedKey, _ := runtimeSessionResourceKey(runtimeAgent(stored))
	if gate == storedKey {
		t.Fatalf("read-only seat gated on the agent's own session key %q", gate)
	}

	// A non-seat job for the same agent still gates on that agent's session, so
	// real work continues to serialize on the session it actually resumes.
	ordinary := newJob("implement-a", false)
	if got := queuedJobRuntimeResourceKey(ctx, store, ordinary); got != storedKey {
		t.Fatalf("ordinary job gate = %q, want the agent session key %q", got, storedKey)
	}
}
