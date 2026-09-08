package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2063: WHAT ACTUALLY HAPPENS WHEN --lead NAMES AN ORG ROLE?
//
// Three seats predicted three different answers and all three were wrong. The
// history is kept here because the labels are the defect this file exists to
// avoid repeating.
//
//	gm-transport predicted: GetAgent returns ErrNoRows, ensureAgentAllowed blocks
//	  with "is not subscribed", so the path is fail-closed but DEAD.
//	gm-integrity (me) first measured: fail-closed, no fix job. That arm did not
//	  vary what its name claimed - see registeredByFixtureSideEffect below.
//	both feared: the fix routes to the REVIEWER, recreating the self-attribution
//	  that #2063 exists to remove.
//
// MEASURED 2026-09-08, all three falsified. A lead that is genuinely absent from
// the agents table does not block and is not honoured: the fix dispatches to the
// agent of the PRIOR IMPLEMENT JOB, and the role is silently ignored for routing.
//
// The trap that produced two wrong answers is in the fixture, not the engine:
// insertCompletedJob CREATES AN AGENT ROW for the job's Agent. Naming the lead as
// the prior implement job's agent therefore REGISTERS it, with policy "auto", and
// the resulting block is the implement-write policy guard - not registration.
// That is why the arms below control the lead's registration explicitly and
// assert it before advancing.

type fixDispatchArm struct {
	// lead is written to payload.LeadAgent on both jobs.
	lead string
	// leadIsPriorImplementer names the lead as the prior implement job's agent,
	// which registers it as a side effect. False keeps it payload-only.
	leadIsPriorImplementer bool
	// seedLead registers the lead explicitly, with a writable policy.
	seedLead bool
}

func advanceChangesRequested(t *testing.T, arm fixDispatchArm) (db.Job, bool, error) {
	t.Helper()
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "builder", []string{"implement"}, "gitmoot/gitmoot")
	if arm.seedLead {
		seedAgent(t, store, arm.lead, []string{"implement"}, "gitmoot/gitmoot")
	}
	engine := testEngine(store)
	engine.RequireWorkflowPolicy = func(string) RequireWorkflowPolicy {
		return RequireWorkflowPolicy{Enabled: true, Mode: "strict"}
	}

	priorImplementer := "builder"
	if arm.leadIsPriorImplementer {
		priorImplementer = arm.lead
	}
	insertCompletedJob(t, store, db.Job{ID: "prior-implement", Agent: priorImplementer, Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-2063", PullRequest: 2063,
		TaskID: "task-2063", LeadAgent: arm.lead,
	})
	// #1712: the fix DISPATCH is an explicit per-PR opt-in. Without this the
	// advancement is report-only and dispatches nothing - which is what the first
	// version of this test silently measured, and why the control exists.
	enableAutoFix(t, store, 2063)
	insertCompletedJob(t, store, db.Job{ID: "review-1", Agent: "audit", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-2063", PullRequest: 2063,
		HeadSHA: strings.Repeat("a", 40), TaskID: "task-2063", TaskTitle: "role lead dispatch",
		LeadAgent: arm.lead, Reviewers: []string{"audit"}, ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "fix the boundary check",
			Findings: []json.RawMessage{json.RawMessage(`{"id":"F-1","severity":"P1","file":"internal/x.go","title":"boundary"}`)},
		},
	})

	// PROVE THE SETUP before measuring, because the fixture registers agents as a
	// side effect and two published answers were wrong about exactly this.
	_, lookupErr := store.GetAgent(ctx, arm.lead)
	registered := lookupErr == nil
	wantRegistered := arm.seedLead || arm.leadIsPriorImplementer
	if registered != wantRegistered {
		t.Fatalf("SETUP INVALID: lead %q registered=%v, arm wants %v", arm.lead, registered, wantRegistered)
	}

	advanceErr := engine.AdvanceJob(ctx, "review-1")
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "implement" && job.ID != "prior-implement" {
			return job, true, advanceErr
		}
	}
	return db.Job{}, false, advanceErr
}

// THE CONTROL, and its own result corrected the model twice. Fix routing does
// NOT follow payload.LeadAgent at all: it follows the PRIOR IMPLEMENT JOB'S
// AGENT. The first version of this control passed only because the fixture made
// those the same agent, which is the same conflation that mislabelled the arm
// below.
func TestFixRoutingFollowsThePriorImplementerNotTheLead(t *testing.T) {
	fix, found, advanceErr := advanceChangesRequested(t, fixDispatchArm{lead: "lead", seedLead: true})
	if !found {
		t.Fatalf("no fix job dispatched, so this harness measures nothing; advance=%v", advanceErr)
	}
	if strings.TrimSpace(fix.Agent) != "builder" {
		t.Fatalf("fix job agent = %q, want the prior implementer %q even though the lead is a different registered agent",
			fix.Agent, "builder")
	}
	// Runtime resolves at EXECUTION, not enqueue, so it is empty for a healthy
	// registered agent too and can never be a hazard signature at this layer.
	t.Logf("control: agent=%q runtime=%q model=%q", fix.Agent, fix.Runtime, fix.Model)
}

// THE FIXTURE TRAP, pinned so it cannot silently mislabel an arm again. Naming
// the lead as the prior implement job's agent REGISTERS it with policy "auto",
// and the dispatch then blocks on the implement-write guard. This arm proves the
// side effect is real; it says nothing about org roles.
func TestLeadRegisteredByFixtureSideEffectBlocksOnPolicy(t *testing.T) {
	_, found, advanceErr := advanceChangesRequested(t, fixDispatchArm{lead: "gm-integrity", leadIsPriorImplementer: true})
	if found {
		t.Fatalf("expected the implement-write policy guard to block, but a fix job was dispatched")
	}
	if advanceErr == nil || !strings.Contains(advanceErr.Error(), "grants no write permission") {
		t.Fatalf("advance error = %v, want the implement-write policy guard", advanceErr)
	}
}

// THE MEASUREMENT. It falsifies every prediction made about this path, including
// mine, and it closes a hazard raised against it.
//
// gm-transport predicted the fix job would block as "not subscribed", making the
// org-role lead FAIL-CLOSED BUT DEAD. It does not block: the fix dispatches
// cleanly to the prior implementer and the role is simply inert.
//
// They then raised a live hazard on top of it: engine_pr_lifecycle.go:53 passes
// event.LeadAgent to eligibleReviewers, which excludes the implementer from its
// own review panel by EXACT STRING EQUALITY, so a role in that field would drop
// nobody and let an implementer review its own head. The route required the role
// to reach a fix job's payload lead and be forwarded from there. It does not: the
// fix job's payload carries the PRIOR IMPLEMENTER, not the role. The last
// assertion here is what keeps that closed - if the role ever starts propagating,
// this fails and the panel-exclusion hazard becomes reachable.
func TestUnregisteredRoleLeadIsInertAndDoesNotPropagate(t *testing.T) {
	fix, found, advanceErr := advanceChangesRequested(t, fixDispatchArm{lead: "gm-integrity"})
	if !found {
		t.Fatalf("no fix job dispatched for an unregistered role lead (advance=%v); "+
			"if this now blocks, the org-role lead path has become DEAD and #2063 needs re-deciding", advanceErr)
	}
	if advanceErr != nil {
		t.Fatalf("advance returned %v, want a clean dispatch", advanceErr)
	}
	if strings.TrimSpace(fix.Agent) == "gm-integrity" {
		t.Fatalf("the org role landed in the fix job's Agent field (%q); a role is not a dispatchable agent", fix.Agent)
	}
	if strings.TrimSpace(fix.Agent) == "audit" {
		t.Fatalf("REGRESSION: the fix was routed to the REVIEWER (%q), which is the self-attribution #2063 removes", fix.Agent)
	}
	payload, err := unmarshalPayload(fix.Payload)
	if err != nil {
		t.Fatalf("unmarshal fix payload: %v", err)
	}
	if strings.TrimSpace(payload.LeadAgent) == "gm-integrity" {
		t.Fatalf("the org role propagated into the fix job's payload lead (%q); from there "+
			"engine_run_budgets.go forwards it to PullRequestEvent.LeadAgent, and the panel exclusion "+
			"at engine_pr_lifecycle.go compares by exact string equality - so the implementer would stay "+
			"eligible to review its own head", payload.LeadAgent)
	}
}
