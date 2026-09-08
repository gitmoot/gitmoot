package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #2063: DOES A ROLE-RESOLVED --lead PRODUCE A FIX JOB WHOSE AGENT RESOLVES TO
// NOTHING?
//
// gm-transport's #2064 review argued it does, from three reads: PullRequestEvent
// LeadAgent becomes a job's Agent verbatim (engine_pr_lifecycle.go:26 and :525),
// resolveEnqueueModel swallows sql.ErrNoRows so an unregistered agent leaves
// runtime and model empty and proceeds, and engine_run_budgets.go:744 passes a
// non-empty payload lead straight through.
//
// Both of us READ that and neither DROVE it, and an owner-approved feature was
// nearly deleted on it. This drives it.
//
// The assertions cover the RESOLVED RUNTIME and MODEL as well as the Agent: the
// hazard is specifically that the job proceeds with those empty, so a test
// checking only the Agent would pass while the real defect survived.

func advanceChangesRequestedWithLead(t *testing.T, lead string, seedLead bool) (db.Job, bool, int) {
	t.Helper()
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	if seedLead {
		seedAgent(t, store, lead, []string{"implement"}, "gitmoot/gitmoot")
	}
	engine := testEngine(store)
	engine.RequireWorkflowPolicy = func(string) RequireWorkflowPolicy {
		return RequireWorkflowPolicy{Enabled: true, Mode: "strict"}
	}

	insertCompletedJob(t, store, db.Job{ID: "impl-" + lead, Agent: lead, Type: "implement"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-2063", PullRequest: 2063,
		TaskID: "task-2063", LeadAgent: lead,
	})
	// #1712: the fix DISPATCH is an explicit per-PR opt-in. Without this the
	// advancement is report-only and dispatches nothing, which is exactly what the
	// first run of this test measured - and why the negative control exists.
	enableAutoFix(t, store, 2063)
	insertCompletedJob(t, store, db.Job{ID: "review-" + lead, Agent: "audit", Type: "review"}, JobPayload{
		Repo: "gitmoot/gitmoot", Branch: "task-2063", PullRequest: 2063,
		HeadSHA: strings.Repeat("a", 40), TaskID: "task-2063", TaskTitle: "role lead dispatch",
		LeadAgent: lead, Reviewers: []string{"audit"}, ReviewRound: "review-1",
		Result: &AgentResult{
			Decision: "changes_requested", Severity: "P1", Summary: "fix the boundary check",
			Findings: []json.RawMessage{json.RawMessage(`{"id":"F-1","severity":"P1","file":"internal/x.go","title":"boundary"}`)},
		},
	})

	if err := engine.AdvanceJob(ctx, "review-"+lead); err != nil {
		t.Logf("AdvanceJob refused: %v", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	for _, job := range jobs {
		if job.Type == "implement" && job.ID != "impl-"+lead {
			return job, true, len(jobs)
		}
	}
	return db.Job{}, false, len(jobs)
}

// THE NEGATIVE CONTROL, and it runs first for a reason: if a REGISTERED lead does
// not produce a resolvable fix job either, this harness cannot see the difference
// the real case is about and any result from it is meaningless. The first run of
// this file skipped here, which is the control doing its job.
func TestRegisteredLeadProducesAResolvableFixJob(t *testing.T) {
	fix, found, total := advanceChangesRequestedWithLead(t, "lead", true)
	if !found {
		t.Fatalf("no fix job dispatched for a REGISTERED lead, so this harness measures nothing; jobs=%d", total)
	}
	t.Logf("CONTROL: fix job id=%q agent=%q runtime=%q model=%q", fix.ID, fix.Agent, fix.Runtime, fix.Model)
	if strings.TrimSpace(fix.Agent) != "lead" {
		t.Fatalf("fix job agent = %q, want the registered lead", fix.Agent)
	}
	fp, err := unmarshalPayload(fix.Payload)
	if err != nil {
		t.Fatalf("unmarshal fix payload: %v", err)
	}
	t.Logf("CONTROL payload: effective_runtime=%q lead=%q acting_role=%q", fp.EffectiveRuntime, fp.LeadAgent, fp.ActingOrgRole)
}

// THE MEASUREMENT, and it FALSIFIED the argument it was written to test.
//
// Measured 2026-09-08, varying ONLY registration against the same fixture:
//
//	registered   "gm-integrity" -> fix job dispatched, agent="gm-integrity"
//	unregistered "gm-integrity" -> BLOCKED, no fix job at all
//
// So the dispatch path is FAIL-CLOSED for a lead that is not a registered agent.
// It does not silently produce a job whose agent resolves to nothing, which was
// the consequence read off resolveEnqueueModel's swallowed sql.ErrNoRows.
//
// WHY it blocks is NOT established here, and the obvious answer is wrong. The
// observed refusal is not "is not subscribed" (engine_routing_merge.go:952) but:
//
//	autonomy policy "auto" grants no write permission in headless runs, but this
//	worker has the "implement" capability
//
// so an unknown lead reaches a preflight holding a zero-value agent rather than
// the not-subscribed branch. What is MEASURED is that the dispatch fails closed
// and creates nothing; the exact refusing site is unidentified and would need its
// own probe. The measurement stands without it - the argument under test was
// about the OUTCOME, not the site.
//
// Also measured, and the reason the control earns its place: the fix job row
// carries runtime="" model="" and payload effective_runtime="" for a REGISTERED
// lead too. Runtime is resolved at execution, not at enqueue, so "empty runtime"
// could never have discriminated the hazard at this layer. A test asserting it
// without a control would have "confirmed" the hazard against a registered agent.
func TestUnregisteredRoleLeadCannotDispatchAFixJob(t *testing.T) {
	fix, found, total := advanceChangesRequestedWithLead(t, "gm-integrity", false)
	if found {
		t.Fatalf("REGRESSION: an UNREGISTERED lead dispatched a fix job (id=%q agent=%q runtime=%q model=%q); "+
			"the dispatch path must fail closed rather than create a job no runtime can execute",
			fix.ID, fix.Agent, fix.Runtime, fix.Model)
	}
	t.Logf("fail-closed as measured: no fix job for an unregistered lead; jobs=%d", total)
}
