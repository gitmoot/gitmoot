package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
)

// TestContinuationPromptAnchorsGoalAndReconcileFraming pins the #418 core: a
// non-empty goal is restated under an "Original goal:" header, the closing is
// reframed to goal-anchored synthesis (reconcile conflicts + flag gaps), and the
// unchanged EMPTY-delegations termination contract is still spelled out.
func TestContinuationPromptAnchorsGoalAndReconcileFraming(t *testing.T) {
	goal := "Compare three sort algorithms and recommend one"
	dels := []Delegation{{ID: "a", Agent: "a", Action: "review"}}
	children := map[string]db.Job{"a": {ID: "p/delegation/a", Agent: "a", State: string(JobSucceeded)}}
	childPayloads := map[string]JobPayload{"a": {Result: &AgentResult{Decision: "done", Summary: "quicksort fastest"}}}

	prompt := Engine{}.buildContinuationPrompt(goal, "", &AgentResult{Delegations: dels}, children, childPayloads, "")

	if !strings.Contains(prompt, "Original goal:\n"+goal) {
		t.Fatalf("continuation prompt missing the Original goal header for %q:\n%s", goal, prompt)
	}
	for _, want := range []string{
		"ORIGINAL GOAL", // goal-anchored synthesis framing
		"Reconcile any conflicts between children",
		"flag any gaps",
		"EMPTY delegations list to finish", // termination contract unchanged
		"Only return new delegations if a genuine gap remains",
		"quicksort fastest", // child results still inlined
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("continuation prompt missing %q:\n%s", want, prompt)
		}
	}
	// The legacy "decide the next step" framing is gone.
	if strings.Contains(prompt, "decide the next step") {
		t.Fatalf("continuation prompt must not retain the legacy 'decide the next step' framing:\n%s", prompt)
	}
}

// TestContinuationPromptOmitsEmptyGoalHeader pins the decided default: an
// empty/whitespace goal omits the "Original goal:" header entirely (never an empty
// header) and degrades to generic synthesis framing while keeping the termination
// contract.
func TestContinuationPromptOmitsEmptyGoalHeader(t *testing.T) {
	dels := []Delegation{{ID: "a", Agent: "a", Action: "review"}}
	children := map[string]db.Job{"a": {ID: "p/delegation/a", Agent: "a", State: string(JobSucceeded)}}
	childPayloads := map[string]JobPayload{"a": {Result: &AgentResult{Decision: "done", Summary: "s"}}}

	for _, goal := range []string{"", "   \n\t  "} {
		prompt := Engine{}.buildContinuationPrompt(goal, "", &AgentResult{Delegations: dels}, children, childPayloads, "")
		if strings.Contains(prompt, "Original goal:") {
			t.Fatalf("empty goal %q must omit the Original goal header:\n%s", goal, prompt)
		}
		// Generic synthesis framing still asks for reconcile/gap + termination.
		for _, want := range []string{
			"Reconcile any conflicts between children",
			"flag any gaps",
			"EMPTY delegations list to finish",
		} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("empty-goal prompt missing %q:\n%s", want, prompt)
			}
		}
		if strings.Contains(prompt, "ORIGINAL GOAL") {
			t.Fatalf("empty-goal prompt must not reference an ORIGINAL GOAL:\n%s", prompt)
		}
	}
}

// TestCorrectiveContinuationPromptAnchorsGoal pins that the corrective nudge also
// restates the goal and carries the goal-anchored synthesis closing.
func TestCorrectiveContinuationPromptAnchorsGoal(t *testing.T) {
	goal := "Find and fix the flaky test"
	prompt := buildCorrectiveContinuationPrompt(goal, &AgentResult{Delegations: []Delegation{{ID: "x", Agent: "w", Action: "ask"}}})
	for _, want := range []string{
		"Original goal:\n" + goal,
		"did not change the outcome", // existing corrective nudge preserved
		"ORIGINAL GOAL",
		"Reconcile any conflicts between children",
		"EMPTY delegations list to finish",
		`delegation "x"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("corrective prompt missing %q:\n%s", want, prompt)
		}
	}
	// Empty goal: no header, generic framing, nudge preserved.
	empty := buildCorrectiveContinuationPrompt("", &AgentResult{})
	if strings.Contains(empty, "Original goal:") {
		t.Fatalf("empty-goal corrective prompt must omit the header:\n%s", empty)
	}
	if !strings.Contains(empty, "did not change the outcome") {
		t.Fatalf("empty-goal corrective prompt dropped the nudge:\n%s", empty)
	}
}

// TestFinalizeContinuationPromptAnchorsGoal pins that the terminal finalize prompt
// restates the goal and asks for a best-effort answer to it, while preserving its
// terminal semantics (any delegations are ignored).
func TestFinalizeContinuationPromptAnchorsGoal(t *testing.T) {
	goal := "Ship the release notes"
	prompt := buildFinalizeContinuationPrompt(goal, &AgentResult{Delegations: []Delegation{{ID: "x", Agent: "w", Action: "ask"}}}, "per-root job budget of 64 reached")
	for _, want := range []string{
		"Original goal:\n" + goal,
		"ORIGINAL GOAL",
		"termination backstop",
		"per-root job budget of 64 reached",
		"reconcile any conflicts between children",
		"EMPTY delegations",
		"ignored", // terminal semantics preserved
		`delegation "x"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("finalize prompt missing %q:\n%s", want, prompt)
		}
	}
}

// TestBudgetPressureLine pins the optional near-bound nudge: it fires within the
// slack of a depth/job bound and stays silent with headroom; an unavailable job
// count (0) suppresses only the job clause.
func TestBudgetPressureLine(t *testing.T) {
	// Plenty of headroom on both axes: silent.
	if got := budgetPressureLine(1, 2); got != "" {
		t.Fatalf("expected no budget-pressure line with headroom, got %q", got)
	}
	// Near the depth bound (depth = MaxDelegationDepth - slack): fires.
	near := budgetPressureLine(MaxDelegationDepth-budgetPressureSlack, 3)
	if !strings.Contains(near, "prefer finishing") {
		t.Fatalf("expected a budget-pressure nudge near the depth bound, got %q", near)
	}
	// Near the job bound only: fires and names the job count.
	jobs := budgetPressureLine(1, MaxDelegationTotalJobs-budgetPressureSlack)
	if !strings.Contains(jobs, "jobs") || !strings.Contains(jobs, "prefer finishing") {
		t.Fatalf("expected a job-count budget-pressure nudge, got %q", jobs)
	}
	// Unavailable job count (0) near depth: depth clause only, no job clause.
	depthOnly := budgetPressureLine(MaxDelegationDepth, 0)
	if !strings.Contains(depthOnly, "depth") || strings.Contains(depthOnly, "jobs") {
		t.Fatalf("expected depth-only nudge with unavailable job count, got %q", depthOnly)
	}

	t.Setenv("GITMOOT_MAX_DELEGATION_DEPTH", "32")
	t.Setenv("GITMOOT_MAX_DELEGATION_TOTAL_JOBS", "128")
	if got := budgetPressureLine(9, 22); got != "" {
		t.Fatalf("expected no pressure with env-raised limits and headroom, got %q", got)
	}
	nearRaised := budgetPressureLine(30, 126)
	if !strings.Contains(nearRaised, "depth 30/32") || !strings.Contains(nearRaised, "126/128 jobs") {
		t.Fatalf("expected pressure line to use env-raised limits, got %q", nearRaised)
	}
}

// TestNestedContinuationAnchorsGoalFromRoot is the load-bearing depth-stability
// test (#418). It drives a real two-generation continuation chain and asserts the
// NESTED continuation's prompt is anchored to the user's ROOT goal — not to the
// parent continuation's built prompt (which parentPayload.Instructions would hold
// for a nested generation). A naive implementation that sourced the goal from
// parentPayload.Instructions would re-anchor the nested prompt to the parent
// continuation's built text (which opens "All delegated jobs have finished") and
// fail the assertions below.
func TestNestedContinuationAnchorsGoalFromRoot(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "coord", []string{"ask"}, "jerryfane/gitmoot")
	seedAgent(t, store, "w", []string{"review"}, "jerryfane/gitmoot")
	engine := testEngine(store)

	const rootGoal = "ROOT_GOAL_SENTINEL: build a tic-tac-toe AI and report its win rate"

	// Generation 0: the root coordinator carries the user's goal in Instructions and
	// delegates one child.
	insertCompletedJob(t, store, db.Job{ID: "root", Agent: "coord", Type: "ask"}, JobPayload{
		Repo:         "jerryfane/gitmoot",
		Branch:       "task-1",
		TaskID:       "task-1",
		Sender:       "coord",
		Instructions: rootGoal,
		Result: &AgentResult{Decision: "approved", Summary: "g0", Delegations: []Delegation{
			{ID: "c0", Agent: "w", Action: "review", Prompt: "do g0 work"},
		}},
	})
	if err := engine.AdvanceJob(ctx, "root"); err != nil {
		t.Fatalf("AdvanceJob(root) error: %v", err)
	}
	completeDelegationChild(t, store, "root/delegation/c0", JobSucceeded, AgentResult{Decision: "approved", Summary: "c0 done"})
	if err := engine.AdvanceJob(ctx, "root/delegation/c0"); err != nil {
		t.Fatalf("AdvanceJob(c0) error: %v", err)
	}

	// Generation 1: the first continuation. Its built Instructions restate the root
	// goal (sanity) and, crucially, it inherits RootJobID == "root". Now make THIS
	// continuation itself delegate again so a nested (gen-2) continuation is born.
	gen1ID := delegationContinuationID("root")
	gen1 := mustJob(t, store, gen1ID)
	gen1Payload, err := unmarshalPayload(gen1.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(gen1) error: %v", err)
	}
	if gen1Payload.RootJobID != "root" {
		t.Fatalf("gen1 continuation RootJobID = %q, want root", gen1Payload.RootJobID)
	}
	if !strings.Contains(gen1Payload.Instructions, rootGoal) {
		t.Fatalf("gen1 continuation prompt should restate the root goal:\n%s", gen1Payload.Instructions)
	}
	// The parent (gen1) continuation prompt opens with this distinctive sentence; if
	// a nested continuation were anchored to parentPayload.Instructions it would leak
	// this into the nested goal section.
	const parentPromptSentinel = "All delegated jobs have finished"
	if !strings.Contains(gen1Payload.Instructions, parentPromptSentinel) {
		t.Fatalf("precondition: gen1 prompt should contain %q:\n%s", parentPromptSentinel, gen1Payload.Instructions)
	}

	gen1Payload.Result = &AgentResult{Decision: "approved", Summary: "g1", Delegations: []Delegation{
		{ID: "c1", Agent: "w", Action: "review", Prompt: "do g1 work"},
	}}
	encoded, err := marshalPayload(gen1Payload)
	if err != nil {
		t.Fatalf("marshalPayload(gen1) error: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, gen1ID, encoded); err != nil {
		t.Fatalf("UpdateJobPayload(gen1) error: %v", err)
	}
	if err := store.UpdateJobState(ctx, gen1ID, string(JobSucceeded)); err != nil {
		t.Fatalf("UpdateJobState(gen1) error: %v", err)
	}
	if err := engine.AdvanceJob(ctx, gen1ID); err != nil {
		t.Fatalf("AdvanceJob(gen1) error: %v", err)
	}
	gen1ChildID := gen1ID + "/delegation/c1"
	completeDelegationChild(t, store, gen1ChildID, JobSucceeded, AgentResult{Decision: "approved", Summary: "c1 done"})
	if err := engine.AdvanceJob(ctx, gen1ChildID); err != nil {
		t.Fatalf("AdvanceJob(gen1 child) error: %v", err)
	}

	// Generation 2: the NESTED continuation. Its goal must resolve from the ROOT, so
	// the goal section equals the root goal verbatim — NOT the parent continuation's
	// built prompt.
	gen2 := mustJob(t, store, delegationContinuationID(gen1ID))
	gen2Payload, err := unmarshalPayload(gen2.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload(gen2) error: %v", err)
	}
	header := "Original goal:\n" + rootGoal + "\n\n"
	if !strings.HasPrefix(gen2Payload.Instructions, header) {
		t.Fatalf("nested continuation must open with the ROOT goal header.\nwant prefix:\n%q\ngot:\n%s", header, gen2Payload.Instructions)
	}
	// Decisive depth-stability check: isolate the goal section (between the header
	// and the body) and assert it is the root goal alone — never the parent prompt.
	goalSection := strings.TrimPrefix(gen2Payload.Instructions, "Original goal:\n")
	goalSection = strings.SplitN(goalSection, "\n\n", 2)[0]
	if strings.TrimSpace(goalSection) != rootGoal {
		t.Fatalf("nested goal section must equal the ROOT goal, got %q (a naive parentPayload.Instructions source would leak the parent prompt here)", goalSection)
	}
	if strings.Contains(goalSection, parentPromptSentinel) {
		t.Fatalf("nested goal section leaked the parent continuation prompt %q:\n%s", parentPromptSentinel, goalSection)
	}
}
