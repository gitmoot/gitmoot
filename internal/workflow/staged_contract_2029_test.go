package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2029 review. Four findings against the staged-dispatch commits, all accepted.
// These cover the three that live in this package; the config-refusal half is in
// internal/cli.
//
// The marker previously did ONE thing: pick which delegation receives
// InheritedEvidence. Everything else about the contract - one delegation, action
// review, that agent, capable, in scope - was stated in the preflight's PROMPT
// and never checked against its RESULT, which is untrusted input.

func stagedContractFixture(t *testing.T, capabilities []string, repos []string) (Engine, *db.Store) {
	t.Helper()
	ctx := context.Background()
	store := openEngineStore(t)
	for _, repo := range repos {
		seedAgent(t, store, "gm-review-opus", capabilities, repo)
	}
	_ = ctx
	return Engine{Store: store}, store
}

func stagedPreflight(delegations []Delegation) (db.Job, JobPayload) {
	job := db.Job{ID: "preflight-2029", Agent: "gm-review-cheap", Type: "review", State: string(JobSucceeded)}
	payload := JobPayload{
		Repo:                     "gitmoot/gitmoot",
		PullRequest:              2029,
		HeadSHA:                  "db4377388a04b683128dfcec5a2012b535f3705e",
		StagedReviewVerdictAgent: "gm-review-opus",
		WorktreePath:             "/tmp/gitmoot-preflight-2029",
		Result: &AgentResult{
			Decision: "approved", Summary: "preflight",
			Evidence:         EvidenceExecuted,
			EvidenceDeclared: true,
			TestsRun:         []string{"go build ./..."},
			Delegations:      delegations,
		},
	}
	return job, payload
}

// THE CENTRAL P1. Each shape below reached a verdict child that received NO
// ceiling, because stagedVerdictCeiling only clamps the delegation whose agent
// matches the marker - while ensureDelegatedReviewEvidence still counted that
// child's approved decision as the fan-out's evidence. The approval then existed
// without the configured strong reviewer running at all.
func TestStagedPreflightRefusesADelegationSetThatIsNotItsVerdictStage(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		delegations []Delegation
		wantReason  string
	}{
		{
			name: "a second, unconstrained sibling",
			delegations: []Delegation{
				{ID: "verdict", Action: "review", Agent: "gm-review-opus"},
				{ID: "extra", Action: "review", Agent: "gm-review-opus"},
			},
			wantReason: "instead of exactly one",
		},
		{
			name:        "an ask child rather than a review",
			delegations: []Delegation{{ID: "verdict", Action: "ask", Agent: "gm-review-opus"}},
			wantReason:  "instead of a review",
		},
		{
			name:        "a review delegated to a different agent",
			delegations: []Delegation{{ID: "verdict", Action: "review", Agent: "some-other-agent"}},
			wantReason:  "delegated to \"some-other-agent\"",
		},
		{
			name:        "a verdict model override",
			delegations: []Delegation{{ID: "verdict", Action: "review", Agent: "gm-review-opus", Model: "cheap-model"}},
			wantReason:  "overrode the configured verdict agent's model",
		},
		{
			name:        "a verdict effort override",
			delegations: []Delegation{{ID: "verdict", Action: "review", Agent: "gm-review-opus", Effort: "low"}},
			wantReason:  "overrode the configured verdict agent's effort",
		},
		{
			// #2029 round two, P1. THIS SUBTEST PREVIOUSLY ASSERTED THE BYPASS AS
			// CORRECT: "no delegations is the existing early return, not a
			// refusal". That expectation was the defect. A marked preflight that
			// approves and delegates NOTHING is not a preflight that did its job -
			// it is an approval with the configured verdict agent never running,
			// reached by emitting nothing rather than the wrong thing.
			name:        "no delegation at all",
			delegations: nil,
			wantReason:  "emitted 0 delegation(s) instead of exactly one",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, store := stagedContractFixture(t, []string{"review"}, []string{"gitmoot/gitmoot"})
			job, payload := stagedPreflight(tc.delegations)

			err := engine.enforceStagedVerdictContract(ctx, job, payload)

			if err == nil {
				t.Fatal("the preflight's delegations were dispatched even though they are not its verdict stage")
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("refusal must name the reason %q, got %q", tc.wantReason, err.Error())
			}
			events, listErr := store.ListJobEvents(ctx, job.ID)
			if listErr != nil {
				t.Fatalf("ListJobEvents: %v", listErr)
			}
			var recorded bool
			for _, ev := range events {
				if ev.Kind == "staged_verdict_contract_refused" {
					recorded = true
				}
			}
			if !recorded {
				t.Fatal("the refusal was not recorded, so it is invisible to an operator")
			}
		})
	}
}

// IDENTITY IS RE-CHECKED AT ADVANCE, not trusted from dispatch. The CLI resolver
// validated the agent when it set the marker, but an agent can lose the
// capability or the repository grant in between, and the preflight's result is
// untrusted input either way.
func TestStagedPreflightRefusesAVerdictAgentThatCannotReviewThisRepo(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		capabilities []string
		repos        []string
		wantReason   string
	}{
		{"no review capability", []string{"implement"}, []string{"gitmoot/gitmoot"}, "does not carry the review capability"},
		{"noncanonical review capability", []string{" Review "}, []string{"gitmoot/gitmoot"}, "does not carry the review capability"},
		{"cannot access the repo", []string{"review"}, []string{"other/repo"}, "cannot access gitmoot/gitmoot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, _ := stagedContractFixture(t, tc.capabilities, tc.repos)
			job, payload := stagedPreflight([]Delegation{{ID: "verdict", Action: "review", Agent: "gm-review-opus"}})

			err := engine.enforceStagedVerdictContract(ctx, job, payload)
			if err == nil {
				t.Fatal("dispatched a verdict stage to an agent that cannot perform it")
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("refusal must name %q, got %q", tc.wantReason, err.Error())
			}
		})
	}
}

// AN UNMARKED JOB IS NOT THIS FUNCTION'S BUSINESS. Without this the enforcement
// would refuse every ordinary fan-out in the fleet, which is a far worse defect
// than the one it fixes.
func TestUnmarkedCoordinatorKeepsDispatchingSeveralDelegations(t *testing.T) {
	ctx := context.Background()
	engine, _ := stagedContractFixture(t, []string{"review"}, []string{"gitmoot/gitmoot"})
	job, payload := stagedPreflight([]Delegation{
		{ID: "lens-a", Action: "review", Agent: "gm-review-opus"},
		{ID: "lens-b", Action: "review", Agent: "gm-review-opus"},
	})
	payload.StagedReviewVerdictAgent = ""

	if err := engine.enforceStagedVerdictContract(ctx, job, payload); err != nil {
		t.Fatalf("an unmarked coordinator's fan-out must be untouched: %v", err)
	}
}

// P1: AN "EXECUTED" CLAIM NAMING NO EXECUTION LIFTS THE CEILING ENTIRELY.
// executed is the permissive value, so this is the honest-looking route to
// removing the whole mechanism - and the general substantiation check is exempt
// here by construction, because it only runs when len(Delegations) == 0.
func TestStagedCeilingRefusesAnUnsubstantiatedExecutedClaim(t *testing.T) {
	verdict := Delegation{ID: "verdict", Action: "review", Agent: "gm-review-opus"}
	_, payload := stagedPreflight([]Delegation{verdict})

	payload.Result.TestsRun = nil
	payload.Result.ChangesMade = nil
	if got := stagedVerdictCeiling(payload, verdict); got != EvidenceStaticOnly {
		t.Fatalf("an executed claim naming nothing gave ceiling %q, want %q", got, EvidenceStaticOnly)
	}

	// Whitespace is not evidence either.
	payload.Result.TestsRun = []string{"   "}
	if got := stagedVerdictCeiling(payload, verdict); got != EvidenceStaticOnly {
		t.Fatalf("a blank tests_run entry counted as execution: ceiling %q", got)
	}

	// #2029 round three, P1: CHANGES_MADE IS NOT EXECUTION EVIDENCE, and this is
	// the reviewer's exact case. evidence=executed with tests_run=[] and
	// changes_made=["no changes"] used to return the PERMISSIVE executed ceiling,
	// because any nonblank changes_made entry counted - so a read-only preflight's
	// prose about what it would change removed the mechanism while naming no
	// command and running nothing.
	//
	// My previous ceiling test left changes_made nil, so it could not tell the two
	// predicates apart and a mutant restoring the old arm survived it.
	payload.Result.TestsRun = nil
	payload.Result.ChangesMade = []string{"no changes"}
	if got := stagedVerdictCeiling(payload, verdict); got != EvidenceStaticOnly {
		t.Fatalf("changes_made prose counted as execution: ceiling %q, want %q", got, EvidenceStaticOnly)
	}
	payload.Result.ChangesMade = nil

	// One real entry is enough. The bar is low on purpose: this separates
	// "executed, and here is what" from "executed" asserted against nothing, and
	// a preflight that ran one command is a legitimate cheap-stage result.
	payload.Result.TestsRun = []string{"go build ./..."}
	if got := stagedVerdictCeiling(payload, verdict); got != EvidenceExecuted {
		t.Fatalf("a substantiated executed claim was clamped to %q", got)
	}

	// A static_only preflight is untouched by this check.
	payload.Result.Evidence = EvidenceStaticOnly
	payload.Result.TestsRun = nil
	if got := stagedVerdictCeiling(payload, verdict); got != EvidenceStaticOnly {
		t.Fatalf("static_only ceiling changed: %q", got)
	}
}

// #2029 ROUND THREE: THE WORKTREE-INHERITANCE TEST IS GONE WITH THE FEATURE IT
// PINNED. Round two made the marked verdict child inherit the preflight's
// WorktreePath so ApplyInheritedEvidenceCeiling's soundness argument would be
// true. The reviewer found the lifetime hole: AdvanceJob has already deferred
// cleanup of the parent-owned read-only worktree, so the parent's own advance
// force-removes the path the queued child was handed, and the child inherited
// neither the read-only seat nor the runtime config directory that make it
// usable. Handing over a directory that is about to be deleted is worse than not
// sharing one, so the inheritance was withdrawn and this test deleted rather than
// weakened to match it.
//
// What remains is recorded honestly in ApplyInheritedEvidenceCeiling's own
// comment: the environment half of its argument is NOT established, the ceiling
// can under-report when the verdict seat is richer than the preflight's, and
// making it sound needs either worktree ownership transferred until the verdict
// child is terminal or a ceiling computed from the child's actual seat. That is a
// design question, escalated rather than papered over.

// AN EXPLICITLY BLOCKED PREFLIGHT IS NOT REFUSED. It emitted no verdict
// delegation because it could not perform the review, which is the honest cheap
// stage answer and is already terminal; turning that into a dispatch error would
// convert a correct refusal into an engine failure. Without this arm, enforcing
// the zero-delegation case would break every legitimate preflight refusal - which
// is the campaign's own #1823 requirement.
func TestBlockedStagedPreflightIsNotRefusedForEmittingNothing(t *testing.T) {
	ctx := context.Background()
	for _, decision := range []string{"blocked", "failed"} {
		t.Run(decision, func(t *testing.T) {
			engine, _ := stagedContractFixture(t, []string{"review"}, []string{"gitmoot/gitmoot"})
			job, payload := stagedPreflight(nil)
			payload.Result.Decision = decision

			if err := engine.enforceStagedVerdictContract(ctx, job, payload); err != nil {
				t.Fatalf("a %s preflight that delegated nothing must not be refused: %v", decision, err)
			}
		})
	}
}

func TestBlockedStagedPreflightCannotWriteFindingsToLedger(t *testing.T) {
	ctx := context.Background()
	for _, decision := range []string{"blocked", "failed"} {
		t.Run(decision, func(t *testing.T) {
			engine, store := stagedContractFixture(t, []string{"review"}, []string{"gitmoot/gitmoot"})
			job, payload := stagedPreflight(nil)
			job.ID += "-" + decision
			payload.Result.Decision = decision
			payload.Result.Findings = []json.RawMessage{
				json.RawMessage(`{"severity":"P1","title":"cheap stage opinion","detail":"must not become an obligation"}`),
			}
			encoded, err := marshalPayload(payload)
			if err != nil {
				t.Fatal(err)
			}
			job.Payload = encoded
			if err := store.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}

			_ = engine.AdvanceJob(ctx, job.ID)
			observations, err := store.ListReviewFindingObservations(ctx, payload.Repo, int64(payload.PullRequest))
			if err != nil {
				t.Fatal(err)
			}
			if len(observations) != 0 {
				t.Fatalf("%s preflight authored findings-ledger obligations: %+v", decision, observations)
			}
		})
	}
}

// A marked preflight with NO RESULT is refused rather than silently ignored: it
// is indistinguishable from a preflight that never ran, and treating it as an
// ordinary review is the same hole.
func TestStagedPreflightWithNoResultIsRefused(t *testing.T) {
	ctx := context.Background()
	engine, _ := stagedContractFixture(t, []string{"review"}, []string{"gitmoot/gitmoot"})
	job, payload := stagedPreflight(nil)
	payload.Result = nil

	err := engine.enforceStagedVerdictContract(ctx, job, payload)
	if err == nil {
		t.Fatal("a marked preflight carrying no result was treated as an ordinary review")
	}
	if !strings.Contains(err.Error(), "no result at all") {
		t.Fatalf("refusal must name the cause, got %q", err.Error())
	}
}

// #2029 ROUND THREE, P1. ENFORCEMENT MUST PRECEDE EVERY REVIEW-RESULT CONSUMER,
// AND THAT IS AN ORDERING CLAIM, SO IT IS TESTED THROUGH AdvanceJob.
//
// The previous round moved enforcement above dispatchDelegations' early return.
// That closed the zero-delegation bypass and left it DOWNSTREAM of everything
// else AdvanceJob does with a review result: a marked preflight could still have
// its findings written to the #1822 ledger and move the task, as an ordinary
// reviewer, before anything checked whether it was allowed to be one.
//
// A contract-level test cannot see this. Only driving the production advance can,
// which is why the shape cases above call the contract directly and this one does
// not.
func TestMarkedPreflightIsRefusedBeforeItsFindingsAreConsumed(t *testing.T) {
	ctx := context.Background()
	engine, store := stagedContractFixture(t, []string{"review"}, []string{"gitmoot/gitmoot"})

	job, payload := stagedPreflight([]Delegation{{ID: "verdict", Action: "review", Agent: "gm-review-opus"}})
	// The cheap stage behaving as a reviewer: its own verdict and its own findings.
	payload.Result.Decision = "changes_requested"
	payload.Result.Findings = []json.RawMessage{
		json.RawMessage(`{"severity":"P1","title":"cheap stage opinion","detail":"should never reach the ledger"}`),
	}
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload: %v", err)
	}
	job.State = string(JobSucceeded)
	job.Payload = encoded
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	advanceErr := engine.AdvanceJob(ctx, job.ID)
	if advanceErr == nil {
		t.Fatal("a marked preflight returning its own changes_requested verdict advanced as an ordinary review")
	}
	if !strings.Contains(advanceErr.Error(), "verdict belongs to the agent it names") &&
		!strings.Contains(advanceErr.Error(), "finding(s) of its own") {
		t.Fatalf("refusal must name the cause, got %q", advanceErr.Error())
	}

	// THE ORDERING ASSERTION: nothing of the preflight's reached the ledger. This
	// is what a contract-level test could not have checked.
	observations, err := store.ListReviewFindingObservations(ctx, "gitmoot/gitmoot", 2029)
	if err != nil {
		t.Fatalf("ListReviewFindingObservations: %v", err)
	}
	if len(observations) != 0 {
		t.Fatalf("the cheap stage's findings entered the ledger before enforcement: %+v", observations)
	}
}

// A marked preflight that declares NO evidence is refused, because
// stagedVerdictCeiling then returns "" - no ceiling at all - so an omitted
// declaration removes the mechanism as effectively as naming the wrong agent.
// Shape and identity were checked; the one field the ceiling is computed FROM was
// not.
func TestMarkedPreflightMustDeclareItsEvidence(t *testing.T) {
	ctx := context.Background()
	engine, _ := stagedContractFixture(t, []string{"review"}, []string{"gitmoot/gitmoot"})
	job, payload := stagedPreflight([]Delegation{{ID: "verdict", Action: "review", Agent: "gm-review-opus"}})
	payload.Result.EvidenceDeclared = false
	payload.Result.Evidence = ""

	err := engine.enforceStagedVerdictContract(ctx, job, payload)
	if err == nil {
		t.Fatal("a preflight that declared no evidence was allowed to dispatch a verdict child with no ceiling")
	}
	if !strings.Contains(err.Error(), "no evidence") {
		t.Fatalf("refusal must name the cause, got %q", err.Error())
	}
}

// #2029 round four, P1. The enforcement moved three times and each placement was
// beaten by a consumer upstream of it. This pins the PROPERTY instead of a
// placement: a marked preflight's result must never be consumable as a review
// verdict by ANY consumer.
//
// The consumer that beat placement three is emitTerminal, called from
// Mailbox.finishWithPayload BEFORE AdvanceJob ever runs. Its review branch
// classifies a SUCCEEDED non-fan-out review result as a verdict and wakes the
// pull request's owner - and a marked preflight returning approved with zero
// delegations is exactly that shape.
func TestMarkedPreflightVerdictIsNotConsumableAtTheTerminalChokepoint(t *testing.T) {
	for _, tc := range []struct {
		name       string
		decision   string
		findings   []json.RawMessage
		wantFailed bool
	}{
		{"approved with zero delegations", "approved", nil, true},
		{"changes_requested of its own", "changes_requested", nil, true},
		{
			"findings of its own",
			"approved",
			[]json.RawMessage{json.RawMessage(`{"severity":"P2","title":"t","detail":"d"}`)},
			true,
		},
		// An explicitly failed preflight is the honest cheap-stage answer and is
		// already terminal; it must pass through untouched.
		{"an honest failure is not refused", "failed", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEngineStore(t)
			seedAgent(t, store, "preflight", []string{"review"}, "gitmoot/gitmoot")
			mailbox := NewMailbox(store, UnavailableDeliveryWorktreeResolver("test"))

			payload := JobPayload{
				Repo: "gitmoot/gitmoot", Branch: "task-2029", PullRequest: 2029,
				TaskID: "task-2029", HeadSHA: strings.Repeat("a", 40),
				StagedReviewVerdictAgent: "verdict-agent",
				Result: &AgentResult{
					Decision: tc.decision, Summary: "preflight says so",
					Evidence: EvidenceExecuted,
					TestsRun: []string{"go test ./internal/workflow/ -> ok"},
					Findings: tc.findings,
				},
			}
			job := db.Job{ID: "preflight-" + strings.ReplaceAll(tc.name, " ", "-"), Agent: "preflight", Type: "review"}
			if err := store.CreateJobWithEvent(ctx, db.Job{
				ID: job.ID, Agent: job.Agent, Type: job.Type, State: string(JobRunning),
				Payload: mustMarshalPayload(t, payload),
			}, db.JobEvent{JobID: job.ID, Kind: string(JobRunning), Message: "seeded"}); err != nil {
				t.Fatalf("CreateJobWithEvent returned error: %v", err)
			}

			if err := mailbox.finishWithPayload(ctx, job.ID, JobSucceeded, "done", payload); err != nil {
				t.Fatalf("finishWithPayload returned error: %v", err)
			}

			stored, err := store.GetJob(ctx, job.ID)
			if err != nil {
				t.Fatalf("GetJob returned error: %v", err)
			}
			if tc.wantFailed {
				if stored.State != string(JobFailed) {
					t.Fatalf("job state = %q, want failed: a succeeded marked preflight is exactly the shape emitTerminal classifies as a verdict, so it must not reach the store succeeded", stored.State)
				}
			} else if stored.State != string(JobSucceeded) {
				t.Fatalf("job state = %q, want succeeded: an honest cheap-stage failure must pass through untouched", stored.State)
			}

			// The reviewer's own text must survive verbatim: only the state changes.
			var back JobPayload
			if err := json.Unmarshal([]byte(stored.Payload), &back); err != nil {
				t.Fatalf("Unmarshal stored payload returned error: %v", err)
			}
			if back.Result == nil || back.Result.Summary != "preflight says so" {
				t.Fatalf("the stored result lost the preflight's own text; refusal must not rewrite what the stage said")
			}
			if back.Result.Decision != tc.decision {
				t.Fatalf("stored decision = %q, want %q verbatim: the refusal changes the state, not the reviewer's words", back.Result.Decision, tc.decision)
			}
		})
	}
}

// An UNMARKED job must be byte-identical: the chokepoint check is scoped to the
// marker and must not touch any ordinary review.
func TestUnmarkedReviewIsUntouchedByTheStagedChokepoint(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "auditor", []string{"review"}, "gitmoot/gitmoot")
	mailbox := NewMailbox(store, UnavailableDeliveryWorktreeResolver("test"))

	payload := JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-plain", PullRequest: 2030,
		TaskID: "task-plain", HeadSHA: strings.Repeat("b", 40),
		Result: &AgentResult{Decision: "approved", Summary: "ordinary review", Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./... -> ok"}},
	}
	if err := store.CreateJobWithEvent(ctx, db.Job{
		ID: "plain-review", Agent: "auditor", Type: "review", State: string(JobRunning),
		Payload: mustMarshalPayload(t, payload),
	}, db.JobEvent{JobID: "plain-review", Kind: string(JobRunning), Message: "seeded"}); err != nil {
		t.Fatalf("CreateJobWithEvent returned error: %v", err)
	}
	if err := mailbox.finishWithPayload(ctx, "plain-review", JobSucceeded, "done", payload); err != nil {
		t.Fatalf("finishWithPayload returned error: %v", err)
	}
	stored, err := store.GetJob(ctx, "plain-review")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobSucceeded) {
		t.Fatalf("an unmarked approved review became %q; the chokepoint check is not scoped to the marker", stored.State)
	}
}

// #2029 round four, P2. A contract refusal used to return from AdvanceJob BEFORE
// the read-only cleanup defer was installed, so the refusal leaked its
// dispatch-allocated worktree and emitted no reclaim marker. Moving the guard
// earlier moved the leak with it, which is why the defer now leads the refusal
// rather than trailing it.
//
// The harness mirrors TestCleanupReadOnlyDelegationWorktreeForceRemoves: cleanup
// no-ops without a DelegationCheckout and a worktree manager, so a fixture
// lacking them measures the harness rather than the behaviour.
func TestStagedContractRefusalStillReclaimsItsReadOnlyWorktree(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "preflight", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	manager := &fakeWorktreeManager{}
	engine.DelegationCheckout = t.TempDir()
	engine.DelegationWorktrees = manager

	wt := managedEngineWorktree(t, &engine, "staged-refusal-delegation-d1")
	payload := JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-refuse", PullRequest: 4243,
		TaskID: "task-refuse", HeadSHA: strings.Repeat("a", 40),
		StagedReviewVerdictAgent: "verdict-agent",
		DelegationID:             "d1",
		WorktreePath:             wt,
		ReadOnlyWorktree:         true,
		Result: &AgentResult{
			// Verdict-shaped with zero delegations: the contract refuses it.
			Decision: "approved", Summary: "preflight", Evidence: EvidenceExecuted,
			TestsRun: []string{"go test ./internal/workflow/ -> ok"},
		},
	}
	insertCompletedJob(t, store, db.Job{ID: "staged-refusal", Agent: "preflight", Type: "review"}, payload)

	err := engine.AdvanceJob(ctx, "staged-refusal")
	if err == nil {
		t.Fatalf("AdvanceJob returned nil; the marked preflight should have been refused")
	}

	// THE REFUSAL MUST NOT COST THE WORKTREE. Force-remove is what the manager
	// records; without the defer above the refusal this list is empty.
	if len(manager.removedForce) != 1 || manager.removedForce[0] != wt {
		t.Fatalf("refusal did not reclaim its read-only worktree: removedForce = %+v, want one force-remove of %q", manager.removedForce, wt)
	}

	// THE EVENT SIDE, because the force-remove call alone is the manager's word.
	// A reclaimed worktree records delegation_worktree_removed; a leaked one that
	// the daemon must pick up later records delegation_worktree_cleanup_skipped.
	// Both are asserted, so this fails whether the refusal leaks silently or
	// leaks loudly.
	//
	// ON-DISK ABSENCE IS DELIBERATELY NOT ASSERTED: fakeWorktreeManager RECORDS a
	// force-remove and does not delete anything, so a directory check here would
	// fail against a correct fix and pass only if the fake grew real filesystem
	// behaviour. The events are the observable this harness can honestly read.
	if got := countJobEvents(t, store, "staged-refusal", "delegation_worktree_removed"); got != 1 {
		t.Fatalf("delegation_worktree_removed count = %d, want 1: the reclaim was not recorded", got)
	}
	if got := countJobEvents(t, store, "staged-refusal", "delegation_worktree_cleanup_skipped"); got != 0 {
		t.Fatalf("delegation_worktree_cleanup_skipped count = %d, want 0: the refusal deferred its cleanup instead of performing it", got)
	}
}

// #2029 round five, P1. A NAMED TARGET INSIDE A DENIAL IS NOT EVIDENCE.
//
// Round four made stagedPreflightNamedExecution reuse namesARunnableTarget, so
// entries naming no target at all - "none run", "nothing to run" - stopped
// lifting the ceiling. The reviewer then showed the reuse answered a NEARBY
// question: "could not run go test ./..." names a real target, so shape alone
// accepted the exact sentence a stage writes when it ran nothing.
//
// The controls matter as much as the arm. A denial list that is too eager
// clamps real evidence, so the false cases below pin executions that must still
// count: a FAILING run is an execution, and a run with skips is an execution.
func TestNegativeProseIsNotExecutionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry string
		want  bool
	}{
		// The reviewer's exact string.
		{"denial naming a real target", "could not run go test ./...", false},
		{"denial, contraction", "couldn't run go test ./internal/workflow/", false},
		{"denial, unable", "unable to run go build ./...", false},
		{"denial, did not", "the suite did not run go test ./...", false},
		{"denial, never started", "failed to run go vet ./internal/cli/", false},
		{"denial, past tense", "go test ./... was not run in this stage", false},
		{"denial with no target at all", "none run", false},

		// #2029 round eight. THE MOST COMMON DENIAL A READ-ONLY SEAT WRITES, and
		// the regex that replaced the literal list silently stopped catching it:
		// "none" does not contain "not". 129 entries in the live ledger take this
		// shape and every one of them escaped between rounds six and eight.
		{"none run, with a target beside it",
			"NONE run in this seat - sandbox denies go build/vet/test on ./...", false},
		{"none runnable", "NONE runnable (sandbox denies go/gh) for ./cmd/gitmoot", false},

		// #2029 round nine f3: THESE ISOLATE THE none-run ALTERNATIVE. Both
		// fixtures above also carry "sandbox denies", so a mutant removing ONLY
		// the none-run and nothing-to-run alternatives passed every focused test:
		// the marker was asserted by fixtures another marker already caught.
		// A FIXTURE THAT TWO RULES BOTH SATISFY TESTS NEITHER.
		{"none run, no other denial marker", "none run for ./cmd/gitmoot this round", false},
		{"nothing to run, no other denial marker", "nothing to run under ./internal/workflow/", false},

		// The reviewer's probe. Measured at ZERO ledger occurrences, where the
		// bare word measures 246 and is refused for that reason.
		{"sandbox blocked, the contextual phrase",
			"sandbox blocked go test ./... before execution", false},
		// #2029 round ten: THE PASSIVE OF THE SAME SENTENCE. Round nine added only
		// the active voice, which is the third time this predicate has been
		// defeated by a voice change rather than by a new claim.
		{"blocked by the sandbox, passive",
			"go test ./... was blocked by the sandbox before execution", false},
		{"blocked by this seat's sandbox", "the probe of ./cmd/gitmoot was blocked by this seat's sandbox", false},

		// AND THE CONTROL THAT REJECTED THE CHEAPER RULE. A real build whose
		// LATER clause mentions the sandbox is an execution: co-occurrence of
		// "blocked" and "sandbox" was measured and rejected because it clamps
		// entries like this one, and a full test run besides.
		{"a real build whose later clause mentions a blocked cgo path",
			"go build ./... -> rc=0 with CGO_ENABLED=0; the cgo path is blocked by the sandbox", true},

		// CLAUSE SPLITTING, NOT THE WINDOW, is what keeps these two words apart.
		// I wrote this fixture believing it pinned the pattern's [^.;:] class, and
		// the mutant proved otherwise: widening that class survived, because
		// deniesExecution splits into clauses before the pattern ever runs. The
		// class was dead and is gone; this case is kept because the BEHAVIOUR is
		// real - a passing run whose next clause mentions a sandbox still counts -
		// but it is guarded by the splitter, and the comment now says so.
		{"blocked by and sandbox straddle a clause boundary",
			"go test ./... -> ok, no cases blocked by policy; sandbox notes follow", true},
		{"static only, sandbox denied", "static verification only: review sandbox denied go build ./... execution", false},

		// MEASURED CONTAMINATION, NOT ADDED. "blocked" was proposed as a denial
		// marker and appears in 243 ledger entries, overwhelmingly as the OUTCOME
		// UNDER TEST rather than as a denial. These are real executions and must
		// keep counting, which is why the list does not grow that way.
		// SAME CLAUSE AS THE TARGET, which is what makes these discriminating. An
		// earlier version of these two put "blocked" in a DIFFERENT clause from the
		// path, so adding "blocked" to the denial list changed no outcome and the
		// mutant survived: the fixture asserted the contamination claim without
		// being able to detect it.
		{"blocked is a result state here, not a denial",
			"go test ./internal/cli/ -> ok, the envelope harness returned blocked JSON as expected", true},
		{"a deploy probe asserting a blocked result",
			"go test ./internal/pipeline/ -> ok, deploy with missing credentials returned blocked without crashing", true},

		// #2029 round six, P1: PASSIVE AND INTERPOSED VOICES. The literal list
		// matched contiguous text and missed every one of these.
		{"passive, the reviewer's exact string", "go test ./... could not be executed in this sandbox", false},
		{"passive, was not able", "go test ./... was not able to be run here", false},
		{"passive, has not been", "go test ./internal/workflow/ has not been executed at this head", false},
		{"never", "go test ./... never ran in this stage", false},
		{"interposed adverb", "go test ./... did not actually run", false},

		// #2029 round six, P1: THE CHARACTER WINDOW WAS THE DEFECT. This entry is
		// one clause and plainly a denial, and its negation-to-verb gap is 41
		// characters, so the previous 32-character window let it lift the ceiling.
		{"long same-clause denial, the reviewer's exact string",
			"go test ./... has not yet been independently and successfully executed at this head", false},
		{"longer still", "go test ./... could not, for reasons of sandbox policy and network isolation, be executed", false},

		// #2029 round seven, P1: THE COMMAND'S OWN VERB DEFEATED FIRST-VERB
		// ANCHORING. In "go run /tmp/tool was not executed" the first verb is part
		// of the COMMAND, so the prefix held no negation and the later "executed"
		// was never examined. Position is gone; a denial anywhere in the clause
		// disqualifies it.
		{"the verb is part of the command", "go run /tmp/tool was not executed", false},

		// THE DENIAL MUST SHARE THE CLAUSE WITH THE TARGET, and a leading clause
		// that names nothing must not answer for the one that does. Without the
		// target check, the first clause here carries no negation and is taken as
		// proof of execution before the denial is ever read.
		{"innocent leading clause, denial in the clause with the target",
			"the branch is ready. go test ./... was not run", false},
		{"same, passive and later", "go run ./cmd/gitmoot has never been executed here", false},

		// THE TRADEOFF, ASSERTED RATHER THAN LEFT IMPLICIT. These two report real
		// executions and are now CLAMPED. That is the safe direction: a false
		// positive keeps the ceiling, a false negative removes it on a sentence
		// saying nothing ran. Three rounds were spent buying this permissiveness
		// back with ordering rules and each purchase reopened the unsafe side.
		{"real run clamped by a later denial", "go test ./... ran, though the linter did not run", false},
		{"real run clamped by a later failure-to-run", "go test ./... ran and then failed to run the second suite", false},

		// BUT A FAILING OR SKIPPING RUN IS STILL AN EXECUTION, because the denial
		// phrases are PHRASES. Matching bare "failed" or "skipped" would clamp
		// every failing suite in the store.
		{"a failing suite", "go test ./internal/workflow/ failed: 2 tests", true},
		{"a suite with skips", "go test ./... -> ok (3 skipped)", true},

		// EXECUTIONS THAT MUST STILL COUNT. A denial list is a clamp, and a clamp
		// that catches these refuses real preflight evidence.
		{"a failing run is an execution", "go test ./... -> FAIL", true},
		{"a failing run, spelled out", "go test ./internal/workflow/ failed: 2 tests", true},
		{"a run with skips is an execution", "go test ./... -> ok (3 skipped)", true},
		{"an ordinary pass", "go test ./... -> ok", true},
		{"a build", "go build ./... -> ok", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := entryClaimsExecution(tc.entry); got != tc.want {
				t.Fatalf("entryClaimsExecution(%q) = %v, want %v", tc.entry, got, tc.want)
			}
			// The staged ceiling reads the same entries, so the two must agree:
			// this is the predicate the P1 was actually about.
			staged := stagedPreflightNamedExecution(AgentResult{TestsRun: []string{tc.entry}})
			if staged != tc.want {
				t.Fatalf("stagedPreflightNamedExecution(tests_run=[%q]) = %v, want %v", tc.entry, staged, tc.want)
			}
		})
	}
}

// namesARunnableTarget keeps answering ITS OWN question, unchanged. The fix
// composes a new predicate rather than narrowing this one, because other
// reasoning depends on "does this name a target" staying separable from "was it
// run" - and a helper that silently answers a different question than its name
// says is how the two halves came to disagree in the first place.
func TestNamesARunnableTargetStillAnswersItsOwnQuestion(t *testing.T) {
	if !namesARunnableTarget("could not run go test ./...") {
		t.Fatalf("namesARunnableTarget must still see the target inside a denial; " +
			"the denial belongs to the evidence question, not to this one")
	}
	if namesARunnableTarget("none run") {
		t.Fatalf("namesARunnableTarget must still reject an entry naming no target")
	}
}

// #2029 round seven, P1. POSITION IS NOT THE PROPERTY.
//
// This replaces TestExecutionDenialRequiresNegationBeforeTheVerb, which pinned
// first-verb anchoring - a rule that was itself defeated by a command whose own
// name contains an execution verb ("go run /tmp/tool was not executed"). Pinning
// it again at a fourth position would ratify the defect.
//
// What survives is that the denial phrases are PHRASES: "failed to run" denies,
// bare "failed" reports a run that failed. That is what keeps the safe-direction
// clamp from swallowing every failing suite in the ledger.
func TestExecutionDenialDistinguishesFailingRunsFromRefusalsToRun(t *testing.T) {
	for _, tc := range []struct {
		entry string
		want  bool
	}{
		{"go test ./... -> FAIL", false},
		{"go test ./internal/workflow/ failed: 2 tests", false},
		{"go test ./... -> ok (3 skipped)", false},
		{"go test ./... failed to run", true},
		{"go test ./... unable to run here", true},
		{"skipped running go test ./...", true},
	} {
		if got := deniesExecution(tc.entry); got != tc.want {
			t.Fatalf("deniesExecution(%q) = %v, want %v: a failing run is an execution, a refusal to run is not",
				tc.entry, got, tc.want)
		}
	}
}

func TestExecutionDenialStopsAtAClauseBoundary(t *testing.T) {
	// The negation belongs to a different clause; the command after the boundary
	// really did run.
	for _, entry := range []string{
		"this is not a regression; go test ./... run clean",
		"the flag is not set. go test ./... executed and passed",
		"not a defect: go test ./internal/cli/ executed",
	} {
		if deniesExecution(entry) {
			t.Fatalf("%q negates a different clause, so the execution after the boundary must still count", entry)
		}
	}
}

// #2029 round five, mutant M4. Agent prose is not reliably lowercase, and every
// fixture above happened to be, so removing the ToLower changed no outcome.
func TestExecutionDenialsIgnoreCase(t *testing.T) {
	for _, entry := range []string{
		"COULD NOT RUN go test ./...",
		"Could Not Run go test ./...",
		"Unable To Run go build ./...",
	} {
		if entryClaimsExecution(entry) {
			t.Fatalf("entry %q denies execution in mixed case and must not lift the ceiling", entry)
		}
	}
}
