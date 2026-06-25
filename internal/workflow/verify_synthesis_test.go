package workflow

import (
	"context"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// seedVerifyCoordinator inserts a completed coordinator job whose delegations are
// a producer leg plus a verify-tagged check leg, mirroring the #421 producer +
// independent-verify shape but tagging the verify leg with synthesis_rule
// "verify" so the engine-level verify gate (#439) is exercised. payload lets a
// caller pre-seed the carried VerifyAttempt (and other fields) on the parent.
func seedVerifyCoordinator(t *testing.T, store *db.Store, payload JobPayload) {
	t.Helper()
	payload.Repo = "jerryfane/gitmoot"
	payload.Branch = "task-005"
	payload.TaskID = "task-5"
	payload.TaskTitle = "Parent"
	payload.Sender = "coord"
	payload.Result = &AgentResult{
		Decision: "approved",
		Summary:  "done",
		Delegations: []Delegation{
			{ID: "produce", Agent: "producer", Action: "implement", Prompt: "build it", FailurePolicy: "continue"},
			{ID: "check", Agent: "verifier", Action: "review", Prompt: "verify it", FailurePolicy: "continue", SynthesisRule: "verify"},
		},
	}
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, payload)
}

// TestEngineVerifySynthesisPassesEnqueuesNormalContinuation pins that when the
// verify-tagged leg approves, the engine enqueues the NORMAL synthesis
// continuation (not a replan or finalize), with no verify_replan_warning event
// and no carried VerifyAttempt.
func TestEngineVerifySynthesisPassesEnqueuesNormalContinuation(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "producer", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "verifier", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	seedVerifyCoordinator(t, store, JobPayload{})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	completeDelegationChild(t, store, "parent-job/delegation/produce", JobSucceeded, AgentResult{Decision: "implemented", Summary: "produced"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/produce"); err != nil {
		t.Fatalf("AdvanceJob(produce) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/check", JobSucceeded, AgentResult{Decision: "approved", Summary: "verified ok"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/check"); err != nil {
		t.Fatalf("AdvanceJob(check) returned error: %v", err)
	}

	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("a passing verify verdict must enqueue the normal continuation")
	}
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("parent-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if cont.DelegationFinalize {
		t.Fatal("a passing verify verdict must NOT enqueue a finalize continuation")
	}
	if cont.VerifyAttempt != 0 {
		t.Fatalf("normal continuation VerifyAttempt = %d, want 0", cont.VerifyAttempt)
	}
	if got := countJobEvents(t, store, "parent-job", "verify_replan_warning"); got != 0 {
		t.Fatalf("verify_replan_warning events = %d, want 0 on a passing verdict", got)
	}
	if got := countJobEvents(t, store, "parent-job", "verify_replan_exhausted"); got != 0 {
		t.Fatalf("verify_replan_exhausted events = %d, want 0 on a passing verdict", got)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_continuation_enqueued"); got != 1 {
		t.Fatalf("delegation_continuation_enqueued events = %d, want 1", got)
	}
	// A passing verify verdict must not block the parent task.
	if state, _ := store.GetTask(ctx, "task-5"); state.State == string(TaskBlocked) {
		t.Fatal("a passing verify verdict must not block the parent")
	}
}

// TestEngineVerifyVerdictFailedEnqueuesBoundedReplan pins that a verify leg with a
// non-approving outcome enqueues exactly ONE bounded corrective replan
// continuation (the deterministic continuation id), emits verify_replan_warning +
// delegation_continuation_enqueued, carries VerifyAttempt=1, and does NOT block
// the parent or enqueue a finalize continuation.
func TestEngineVerifyVerdictFailedEnqueuesBoundedReplan(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "producer", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "verifier", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	seedVerifyCoordinator(t, store, JobPayload{})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	completeDelegationChild(t, store, "parent-job/delegation/produce", JobSucceeded, AgentResult{Decision: "implemented", Summary: "produced"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/produce"); err != nil {
		t.Fatalf("AdvanceJob(produce) returned error: %v", err)
	}
	// The verify leg requests changes: the verdict FAILS.
	completeDelegationChild(t, store, "parent-job/delegation/check", JobSucceeded, AgentResult{Decision: "changes_requested", Summary: "test still red"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/check"); err != nil {
		t.Fatalf("AdvanceJob(check) returned error: %v", err)
	}

	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("a failed verify verdict must enqueue a corrective replan continuation")
	}
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("parent-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if cont.DelegationFinalize {
		t.Fatal("a failed verdict below the cap must replan, not finalize")
	}
	if cont.VerifyAttempt != 1 {
		t.Fatalf("replan continuation VerifyAttempt = %d, want 1", cont.VerifyAttempt)
	}
	if got := countJobEvents(t, store, "parent-job", "verify_replan_warning"); got != 1 {
		t.Fatalf("verify_replan_warning events = %d, want 1", got)
	}
	if got := countJobEvents(t, store, "parent-job", "verify_replan_exhausted"); got != 0 {
		t.Fatalf("verify_replan_exhausted events = %d, want 0 below the cap", got)
	}
	// The replan IS the single continuation: exactly one slot event.
	if got := countJobEvents(t, store, "parent-job", "delegation_continuation_enqueued"); got != 1 {
		t.Fatalf("delegation_continuation_enqueued events = %d, want 1", got)
	}
	// A failed verify verdict must NOT block the parent (autonomous self-correction).
	if state, _ := store.GetTask(ctx, "task-5"); state.State == string(TaskBlocked) {
		t.Fatal("a failed verify verdict must not block the parent")
	}
}

// TestEngineVerifyReplanAttemptCapRoutesToFinalize pins that when the carried
// VerifyAttempt has reached the cap and the verdict fails again, the engine routes
// to the #305 graceful finalize continuation (DelegationFinalize set,
// verify_replan_exhausted event) rather than enqueueing another replan.
func TestEngineVerifyReplanAttemptCapRoutesToFinalize(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "producer", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "verifier", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	// Pre-seed the parent at the default cap (2): a further failure must finalize.
	seedVerifyCoordinator(t, store, JobPayload{VerifyAttempt: defaultMaxVerifyReplanAttempts})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	completeDelegationChild(t, store, "parent-job/delegation/produce", JobSucceeded, AgentResult{Decision: "implemented", Summary: "produced"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/produce"); err != nil {
		t.Fatalf("AdvanceJob(produce) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/check", JobSucceeded, AgentResult{Decision: "changes_requested", Summary: "still failing"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/check"); err != nil {
		t.Fatalf("AdvanceJob(check) returned error: %v", err)
	}

	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("parent-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("at the cap a failed verdict must route to a finalize continuation: %+v", cont)
	}
	if got := countJobEvents(t, store, "parent-job", "verify_replan_exhausted"); got != 1 {
		t.Fatalf("verify_replan_exhausted events = %d, want 1", got)
	}
	if got := countJobEvents(t, store, "parent-job", "verify_replan_warning"); got != 0 {
		t.Fatalf("verify_replan_warning events = %d, want 0 at the cap", got)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_finalize_enqueued"); got != 1 {
		t.Fatalf("delegation_finalize_enqueued events = %d, want 1", got)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_continuation_enqueued"); got != 1 {
		t.Fatalf("delegation_continuation_enqueued events = %d, want 1", got)
	}
}

// TestEngineVerifyContinuationSlotIdempotent pins the single-continuation-slot
// invariant: re-advancing the same parent after a failed verify verdict does not
// double-enqueue (the continuationEnqueued top-guard + deterministic id), asserted
// via both the event count and the absence of a second continuation job.
func TestEngineVerifyContinuationSlotIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "producer", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "verifier", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	seedVerifyCoordinator(t, store, JobPayload{})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/produce", JobSucceeded, AgentResult{Decision: "implemented", Summary: "produced"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/produce"); err != nil {
		t.Fatalf("AdvanceJob(produce) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/check", JobSucceeded, AgentResult{Decision: "changes_requested", Summary: "fix me"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/check"); err != nil {
		t.Fatalf("AdvanceJob(check) returned error: %v", err)
	}

	// Re-advance the parent and both children: none may enqueue a second
	// continuation or re-emit the warning.
	for _, id := range []string{"parent-job", "parent-job/delegation/produce", "parent-job/delegation/check"} {
		if err := engine.AdvanceJob(ctx, id); err != nil {
			t.Fatalf("re-AdvanceJob(%s) returned error: %v", id, err)
		}
	}

	if got := countJobEvents(t, store, "parent-job", "verify_replan_warning"); got != 1 {
		t.Fatalf("after re-advance: verify_replan_warning events = %d, want 1", got)
	}
	if got := countJobEvents(t, store, "parent-job", "delegation_continuation_enqueued"); got != 1 {
		t.Fatalf("after re-advance: delegation_continuation_enqueued events = %d, want 1", got)
	}
	// Only the deterministic continuation id exists; no second continuation job.
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	continuations := 0
	for _, j := range jobs {
		if j.ParentJobID == "parent-job" && j.DelegationID == "" {
			continuations++
		}
	}
	if continuations != 1 {
		t.Fatalf("continuation jobs under parent = %d, want exactly 1", continuations)
	}
}

// TestEngineVerifyReplanAttemptCapConfigurable pins that Engine.MaxVerifyReplanAttempts
// overrides the default-2 fallback: with the cap set to 1, a parent already at
// VerifyAttempt=1 finalizes instead of replanning (mirroring the non-progress
// streak configurability test).
func TestEngineVerifyReplanAttemptCapConfigurable(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "producer", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "verifier", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)
	engine.MaxVerifyReplanAttempts = 1

	// VerifyAttempt=1 is below the default cap (2) but AT the configured cap (1),
	// so a failed verdict must finalize, proving the knob overrode the default.
	seedVerifyCoordinator(t, store, JobPayload{VerifyAttempt: 1})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/produce", JobSucceeded, AgentResult{Decision: "implemented", Summary: "produced"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/produce"); err != nil {
		t.Fatalf("AdvanceJob(produce) returned error: %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/check", JobSucceeded, AgentResult{Decision: "changes_requested", Summary: "nope"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/check"); err != nil {
		t.Fatalf("AdvanceJob(check) returned error: %v", err)
	}

	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("parent-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if !cont.DelegationFinalize {
		t.Fatalf("with cap=1 and VerifyAttempt=1 a failed verdict must finalize: %+v", cont)
	}
	if got := countJobEvents(t, store, "parent-job", "verify_replan_exhausted"); got != 1 {
		t.Fatalf("verify_replan_exhausted events = %d, want 1 under cap=1", got)
	}
}

// TestEngineVerifyMissingChildFailsVerdict pins that a verify-tagged leg with no
// completed child (a missing verification) fails the verdict and triggers a replan
// rather than being read as a pass.
func TestEngineVerifyMissingChildFailsVerdict(t *testing.T) {
	children := map[string]db.Job{}
	childPayloads := map[string]JobPayload{}
	dels := []Delegation{
		{ID: "check", Agent: "verifier", Action: "review", Prompt: "verify", SynthesisRule: "verify"},
	}
	// The verify leg here has no deps, so it WAS dispatchable: a missing child is a
	// genuinely missing verification and must fail the verdict.
	if verifyVerdictPassed(dels, children, childPayloads, nil) {
		t.Fatal("a missing verify child must fail the verdict")
	}
	// A succeeded verify child passes; a changes_requested decision fails.
	children["check"] = db.Job{ID: "c", State: string(JobSucceeded)}
	if !verifyVerdictPassed(dels, children, childPayloads, nil) {
		t.Fatal("a succeeded verify child must pass the verdict")
	}
	children["check"] = db.Job{ID: "c", State: string(JobFailed)}
	childPayloads["check"] = JobPayload{Result: &AgentResult{Decision: "changes_requested"}}
	if verifyVerdictPassed(dels, children, childPayloads, nil) {
		t.Fatal("a changes_requested verify child must fail the verdict")
	}
	childPayloads["check"] = JobPayload{Result: &AgentResult{Decision: "approved"}}
	children["check"] = db.Job{ID: "c", State: string(JobFailed)}
	if !verifyVerdictPassed(dels, children, childPayloads, nil) {
		t.Fatal("an approved verify decision must pass the verdict even when the job state is not succeeded")
	}
}

// seedDepsBoundVerifyCoordinator inserts a completed coordinator whose verify leg
// DEPENDS ON the producer (the canonical decompose-and-verify shape: the verifier
// only runs after the producer succeeds). producerPolicy sets the producer's
// failure_policy so a caller can exercise both the "continue" and "escalate"
// upstream-failure paths in which the producer fails and the deps-bound verify leg
// therefore never gets a child.
func seedDepsBoundVerifyCoordinator(t *testing.T, store *db.Store, producerPolicy string) {
	t.Helper()
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:      "jerryfane/gitmoot",
		Branch:    "task-005",
		TaskID:    "task-5",
		TaskTitle: "Parent",
		Sender:    "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "done",
			Delegations: []Delegation{
				{ID: "produce", Agent: "producer", Action: "implement", Prompt: "build it", FailurePolicy: producerPolicy},
				{ID: "check", Agent: "verifier", Action: "review", Prompt: "verify it", Deps: []string{"produce"}, SynthesisRule: "verify"},
			},
		},
	})
}

// TestEngineVerifyLegNeverRanUnderContinueDoesNotReplan pins the #439 review fix:
// when the producer fails under failure_policy "continue", the deps-bound verify
// leg never enqueues (it is permanently blocked), so the verify VERDICT must NOT be
// read as a failure. The engine must fall through to the normal continuation — no
// verify_replan_warning, no carried VerifyAttempt — letting the upstream failure
// policy drive the outcome rather than fabricating a failed verdict from a verifier
// that never ran.
func TestEngineVerifyLegNeverRanUnderContinueDoesNotReplan(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "producer", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "verifier", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	seedDepsBoundVerifyCoordinator(t, store, "continue")
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	// The producer fails under continue: the deps-bound verify leg is permanently
	// blocked and never enqueues, so the batch resolves and the continuation fires.
	completeDelegationChild(t, store, "parent-job/delegation/produce", JobFailed, AgentResult{Decision: "failed", Summary: "producer broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/produce"); err != nil {
		t.Fatalf("AdvanceJob(produce) returned error: %v", err)
	}

	// The verify child must never have been enqueued.
	if jobExists(t, store, "parent-job/delegation/check") {
		t.Fatal("the deps-bound verify leg must not enqueue after its producer failed")
	}
	// The continuation IS enqueued (batch resolved), but it is the NORMAL one.
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("the continue-gated batch must enqueue a continuation once it resolves")
	}
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("parent-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if cont.DelegationFinalize {
		t.Fatal("a verify leg that never ran must NOT route to a finalize continuation")
	}
	if cont.VerifyAttempt != 0 {
		t.Fatalf("a verify leg that never ran must NOT burn a verify attempt: VerifyAttempt = %d, want 0", cont.VerifyAttempt)
	}
	if got := countJobEvents(t, store, "parent-job", "verify_replan_warning"); got != 0 {
		t.Fatalf("verify_replan_warning events = %d, want 0 when the verify leg never ran", got)
	}
	if got := countJobEvents(t, store, "parent-job", "verify_replan_exhausted"); got != 0 {
		t.Fatalf("verify_replan_exhausted events = %d, want 0 when the verify leg never ran", got)
	}
}

// TestEngineVerifyLegNeverRanUnderEscalateDoesNotReplan pins the same #439 review
// fix on the escalate path: the producer fails under failure_policy "escalate", so
// the engine enqueues the continuation immediately (before the deps-bound verify
// leg could ever run). With no verify child, the verdict must be SKIPPED (the leg
// is permanently blocked), not fabricated as a failure — no verify_replan_warning,
// no burned VerifyAttempt — leaving the escalate path to drive the outcome.
func TestEngineVerifyLegNeverRanUnderEscalateDoesNotReplan(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "producer", []string{"implement"}, "jerryfane/gitmoot")
	seedAgent(t, store, "verifier", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	seedDepsBoundVerifyCoordinator(t, store, "escalate")
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent) returned error: %v", err)
	}

	// The producer fails under escalate: the continuation is enqueued immediately,
	// before the deps-bound verify leg ever runs.
	completeDelegationChild(t, store, "parent-job/delegation/produce", JobFailed, AgentResult{Decision: "failed", Summary: "producer broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/produce"); err != nil {
		t.Fatalf("AdvanceJob(produce) returned error: %v", err)
	}

	if jobExists(t, store, "parent-job/delegation/check") {
		t.Fatal("the deps-bound verify leg must not enqueue after its producer failed under escalate")
	}
	if !jobExists(t, store, delegationContinuationID("parent-job")) {
		t.Fatal("escalate must enqueue a continuation immediately on producer failure")
	}
	cont, err := unmarshalPayload(mustJob(t, store, delegationContinuationID("parent-job")).Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if cont.DelegationFinalize {
		t.Fatal("a verify leg that never ran (escalate) must NOT route to a finalize continuation")
	}
	if cont.VerifyAttempt != 0 {
		t.Fatalf("a verify leg that never ran (escalate) must NOT burn a verify attempt: VerifyAttempt = %d, want 0", cont.VerifyAttempt)
	}
	if got := countJobEvents(t, store, "parent-job", "verify_replan_warning"); got != 0 {
		t.Fatalf("verify_replan_warning events = %d, want 0 when the verify leg never ran (escalate)", got)
	}
	if got := countJobEvents(t, store, "parent-job", "verify_replan_exhausted"); got != 0 {
		t.Fatalf("verify_replan_exhausted events = %d, want 0 when the verify leg never ran (escalate)", got)
	}
}

// TestValidateDelegationVerifySynthesisRule pins that synthesis_rule "verify" is
// accepted with no Quorum field (unlike quorum), that it does not trigger the
// quorum-count guard at extraction, and that an unknown rule still errors.
func TestValidateDelegationVerifySynthesisRule(t *testing.T) {
	if err := validateDelegationLifecycle(Delegation{ID: "d", SynthesisRule: "verify"}); err != nil {
		t.Fatalf("validateDelegationLifecycle rejected verify (no Quorum required): %v", err)
	}
	// An unknown rule still errors.
	if err := validateDelegationLifecycle(Delegation{ID: "d", SynthesisRule: "coinflip"}); err == nil {
		t.Fatal("validateDelegationLifecycle accepted an unknown synthesis_rule")
	}
	// A verify delegation must NOT trip the quorum-threshold guard at extraction
	// (that guard only applies to synthesis_rule quorum).
	result := AgentResult{
		Decision: "approved",
		Summary:  "s",
		Delegations: []Delegation{
			{ID: "a", Agent: "a", Action: "ask", Prompt: "p"},
			{ID: "b", Agent: "b", Action: "review", Prompt: "p", SynthesisRule: "verify"},
		},
	}
	if err := validateAgentResult(result); err != nil {
		t.Fatalf("validateAgentResult rejected a verify delegation: %v", err)
	}
}
