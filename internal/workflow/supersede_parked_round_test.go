package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// TestParentOnlyAdvanceRefusesAParkedRepairRound pins the guard the merge left the
// parent-only advance outside of.
//
// A round parked in needs_repair means a human's decision was CLAIMED and never
// applied. AdvanceJob refuses such an advance as its first action after loading the
// payload (escalationRepairBlock); AdvanceParentDAGForTerminalChild did not consult the
// round at all, so a closed-PR-superseded sibling advanced the coordinator's delegation
// DAG straight past it. The gap is merge-created by construction: escalationRepairBlock
// existed only on this branch's parent, the parent-only operation only on main's.
//
// THE ASSERTION IS THE JOB COUNT, not the returned error, and that is deliberate: the
// retry pass inside advanceDelegations runs BEFORE any failure policy, so a mutant that
// removes only the guard's error return still lands a fresh queued child and then ends
// the call with AwaitingHumanError - which isDelegationPolicyOutcome classifies as a
// policy outcome, so the supersede recovery would record its debt PAID over the top of
// it. The enqueue is the harm; the error is just how it announces itself.
func TestParentOnlyAdvanceRefusesAParkedRepairRound(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "gitmoot/gitmoot")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-5", RepoFullName: "gitmoot/gitmoot", Branch: "task-005", GoalID: "g1",
		Title: "Parent", State: string(TaskImplementing),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	// api escalates to a human on failure; ui carries a RETRY BUDGET, which is what
	// makes the bypass observable as a new job rather than only as a wrong error.
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-005", PullRequest: 5, TaskID: "task-5", LeadAgent: "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "escalate_human"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui", Retry: 1},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent-job): %v", err)
	}

	// api fails -> the human round opens.
	completeDelegationChild(t, store, "parent-job/delegation/api", JobFailed,
		AgentResult{Decision: "failed", Summary: "api broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected the escalate_human leg to open a round")
	}

	// PARK THE ROUND: a claimed resolution whose effects keep failing past the bound is
	// left in needs_repair with the claim preserved.
	boom := errors.New("transient dependency outage")
	resolutionEffectsHook = func(context.Context, string) error { return boom }
	t.Cleanup(func() { resolutionEffectsHook = nil })
	if err := engine.ResolveEscalation(ctx, "parent-job", ResumeContinue, ""); !errors.Is(err, boom) {
		t.Fatalf("ResolveEscalation error = %v, want the injected failure", err)
	}
	for i := 0; i < escalationRecoveryAttemptBound+2; i++ {
		if _, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil && !errors.Is(err, boom) {
			t.Fatalf("recovery pass %d: %v", i, err)
		}
	}
	round, ok := unsettledRound(t, store, "parent-job")
	if !ok || !round.NeedsRepair() {
		t.Fatalf("round not parked: ok=%v integrity=%q; this test must start from a needs_repair round", ok, round.IntegrityState)
	}

	// ui is terminalized as a closed-PR supersession, the production shape this recovery
	// re-drives.
	const sibling = "parent-job/delegation/ui"
	observed := mustJob(t, store, sibling)
	terminal, err := store.TransitionJobStateWithEventAtGeneration(ctx, sibling, observed.State, observed.LifecycleGeneration, string(JobFailed),
		db.JobEvent{JobID: sibling, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"})
	if err != nil || !terminal {
		t.Fatalf("terminalize sibling: transitioned=%v err=%v", terminal, err)
	}
	stampSupersededResult(t, store, sibling, "pr closed")

	before := countJobs(t, store, "")
	advanceErr := engine.AdvanceParentDAGForTerminalChild(ctx, sibling)
	after := countJobs(t, store, "")

	if after != before {
		t.Fatalf("job count %d -> %d: the parent-only advance ran the coordinator's delegation DAG past a needs_repair round "+
			"and enqueued work while a human's claimed decision was never applied (err = %v)", before, after, advanceErr)
	}
	var blocked BlockedError
	if !errors.As(advanceErr, &blocked) {
		t.Fatalf("error = %v, want BlockedError: the refusal must be classifiable by the daemon worker the same way AdvanceJob's is", advanceErr)
	}
	if _, err := store.GetJob(ctx, sibling+"/retry/1"); err == nil {
		t.Fatal("the retry child was enqueued: the retry pass runs before any failure policy, so this is the harm the guard exists to stop")
	}
}

// stampSupersededResult writes the PRODUCTION result shape a closed-PR supersession
// leaves behind: decision failed, with the supersession marker set.
func stampSupersededResult(t *testing.T, store *db.Store, jobID string, summary string) {
	t.Helper()
	job := mustJob(t, store, jobID)
	payload, err := unmarshalPayload(job.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload: %v", err)
	}
	payload.Result = &AgentResult{
		Decision:                    "failed",
		Summary:                     summary,
		SupersededPullRequestClosed: true,
	}
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	if err := store.UpdateJobPayload(context.Background(), jobID, encoded); err != nil {
		t.Fatalf("UpdateJobPayload: %v", err)
	}
}

// TestSupersedeRecoveryRefusesAParkedRepairRound enters through the PRODUCTION route
// rather than calling the parent-only operation directly: advanceSupersededChildAtGeneration
// is what the debt sweep re-drives, and it is the caller whose bypass mattered.
//
// It also pins the consequence the direct-entry test cannot see: the supersede recovery
// classifies AwaitingHumanError as a policy outcome, so with the guard missing it would
// confirm the bracket and record the finalization debt PAID over the top of an enqueue
// that should never have happened.
func TestSupersedeRecoveryRefusesAParkedRepairRound(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	sibling := seedRoundWithSupersededSibling(t, store, engine, true)

	before := countJobs(t, store, "")
	observed := mustJob(t, store, sibling)
	advanced, err := engine.advanceSupersededChildAtGeneration(ctx, sibling, observed.LifecycleGeneration)
	after := countJobs(t, store, "")

	if after != before {
		t.Fatalf("job count %d -> %d: the supersede recovery advanced the coordinator's DAG past a needs_repair round (advanced=%v err=%v)",
			before, after, advanced, err)
	}
	if _, gerr := store.GetJob(ctx, sibling+"/retry/1"); gerr == nil {
		t.Fatal("the retry child was enqueued through the recovery path")
	}
	// The debt must NOT be settled: a refused advance owes the same work next poll.
	if got := countWorkflowJobEvents(t, store, sibling, JobEventSupersedeFinalizeCompleted); got != 0 {
		t.Fatalf("%s events = %d, want 0: the debt was recorded paid although the advance was refused", JobEventSupersedeFinalizeCompleted, got)
	}
}

// TestSupersedeRecoveryAdvancesWhenNoRoundIsParked is the VALID-SUCCESS CONTROL, and it
// is the half that stops the guard from being a bound that rejects legitimate work: the
// identical tree with its round RESOLVED must advance and settle normally. A guard that
// refused unconditionally would satisfy both tests above and break every ordinary
// recovery.
func TestSupersedeRecoveryAdvancesWhenNoRoundIsParked(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	engine := testEngine(store)
	engine.EscalationNotifier = &recordingNotifier{}
	sibling := seedRoundWithSupersededSibling(t, store, engine, false)

	observed := mustJob(t, store, sibling)
	advanced, err := engine.advanceSupersededChildAtGeneration(ctx, sibling, observed.LifecycleGeneration)
	if err != nil && !isDelegationPolicyOutcome(err) {
		t.Fatalf("advanceSupersededChildAtGeneration on a repaired tree: %v", err)
	}
	if !advanced {
		t.Fatal("a recovery with no parked round was refused: the guard rejects valid work")
	}
	if got := countWorkflowJobEvents(t, store, sibling, JobEventSupersedeAdvanceConfirmed); got != 1 {
		t.Fatalf("%s events = %d, want 1 on the success path", JobEventSupersedeAdvanceConfirmed, got)
	}
}

// seedRoundWithSupersededSibling builds the reviewer's fixture: a coordinator
// fanning out an escalate_human leg and a retry-budgeted leg, the first failing to open
// a human round that is then parked in needs_repair, and the second terminalized as a
// closed-PR supersession with the production result shape. It returns the sibling id.
func seedRoundWithSupersededSibling(t *testing.T, store *db.Store, engine Engine, park bool) string {
	t.Helper()
	ctx := context.Background()
	seedAgent(t, store, "coord", []string{"ask"}, "gitmoot/gitmoot")
	seedAgent(t, store, "api", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "ui", []string{"review"}, "gitmoot/gitmoot")
	if err := store.UpsertTask(ctx, db.Task{
		ID: "task-5", RepoFullName: "gitmoot/gitmoot", Branch: "task-005", GoalID: "g1",
		Title: "Parent", State: string(TaskImplementing),
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	insertCompletedJob(t, store, db.Job{ID: "parent-job", Agent: "coord", Type: "ask"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-005", PullRequest: 5, TaskID: "task-5", LeadAgent: "coord",
		Result: &AgentResult{
			Decision: "approved",
			Summary:  "fan out",
			Delegations: []Delegation{
				{ID: "api", Agent: "api", Action: "review", Prompt: "build api", FailurePolicy: "escalate_human"},
				{ID: "ui", Agent: "ui", Action: "review", Prompt: "build ui", Retry: 1},
			},
		},
	})
	if err := engine.AdvanceJob(ctx, "parent-job"); err != nil {
		t.Fatalf("AdvanceJob(parent-job): %v", err)
	}
	completeDelegationChild(t, store, "parent-job/delegation/api", JobFailed,
		AgentResult{Decision: "failed", Summary: "api broke"})
	if err := engine.AdvanceJob(ctx, "parent-job/delegation/api"); err == nil {
		t.Fatal("expected the escalate_human leg to open a round")
	}
	// THE ONE FACT THAT DIFFERS between the refusal test and its success control: whether
	// the human's claimed decision was applied. Everything else - tree shape, retry
	// budget, supersession, result payload - is identical.
	if park {
		boom := errors.New("transient dependency outage")
		resolutionEffectsHook = func(context.Context, string) error { return boom }
		t.Cleanup(func() { resolutionEffectsHook = nil })
		if err := engine.ResolveEscalation(ctx, "parent-job", ResumeContinue, ""); !errors.Is(err, boom) {
			t.Fatalf("ResolveEscalation error = %v, want the injected failure", err)
		}
		for i := 0; i < escalationRecoveryAttemptBound+2; i++ {
			if _, err := engine.RecoverUnfinishedEscalationResolutions(ctx); err != nil && !errors.Is(err, boom) {
				t.Fatalf("recovery pass %d: %v", i, err)
			}
		}
		resolutionEffectsHook = nil
		round, ok := unsettledRound(t, store, "parent-job")
		if !ok || !round.NeedsRepair() {
			t.Fatalf("round not parked: ok=%v integrity=%q", ok, round.IntegrityState)
		}
	} else {
		if err := engine.ResolveEscalation(ctx, "parent-job", ResumeContinue, ""); err != nil && !isDelegationPolicyOutcome(err) {
			t.Fatalf("ResolveEscalation on the control tree: %v", err)
		}
		if round, ok := unsettledRound(t, store, "parent-job"); ok && round.NeedsRepair() {
			t.Fatal("control fixture drift: the round is parked, so this is not a success control")
		}
	}

	const sibling = "parent-job/delegation/ui"
	observed := mustJob(t, store, sibling)
	terminal, err := store.TransitionJobStateWithEventAtGeneration(ctx, sibling, observed.State, observed.LifecycleGeneration, string(JobFailed),
		db.JobEvent{JobID: sibling, Kind: JobEventSupersededPullRequestClosed, Message: "pr closed"},
		db.JobEvent{JobID: sibling, Kind: JobEventSupersedeFinalizePending, Message: formatSupersedeFinalizeDebt(observed.LifecycleGeneration)})
	if err != nil || !terminal {
		t.Fatalf("terminalize sibling: transitioned=%v err=%v", terminal, err)
	}
	stampSupersededResult(t, store, sibling, "pr closed")
	return sibling
}
