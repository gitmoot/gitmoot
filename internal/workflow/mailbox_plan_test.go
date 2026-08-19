package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

const planResultJSON = `{"gitmoot_result":{"decision":"approved","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

// TestMailboxEnqueueRejectsImpossiblePlanRequest pins the two plan shapes that
// can never run, refused at the enqueue chokepoint BEFORE a job row exists
// (#1479). Kills the silent-drop defect: enqueuing either shape and letting the
// worker quietly run an ordinary implementation turns a plan-gated brief into an
// unplanned one with no signal anywhere.
func TestMailboxEnqueueRejectsImpossiblePlanRequest(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}

	cases := map[string]struct {
		request  JobRequest
		wantText string
	}{
		"plan_into without plan": {
			request:  JobRequest{ID: "job-plan-bad", Agent: "a", Action: "ask", Repo: "o/r", PlanInto: "@smol"},
			wantText: "plan_into requires plan mode",
		},
		"plan on a runtime override that cannot plan": {
			request: JobRequest{ID: "job-plan-bad", Agent: "a", Action: "ask", Repo: "o/r", Plan: true,
				RuntimeOverride: runtime.CodexRuntime, RuntimeOverrideRef: "codex-session"},
			wantText: "cannot honour plan mode",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// The ID is derived from the case name because `cases` is a map and Go
			// randomises map iteration: a shared ID on a shared store meant that
			// when the guard actually regressed, one subtest reported the other's
			// leftover row (or a UNIQUE-constraint error) and the diagnosis landed
			// on the wrong plan shape in ~1 run in 5. It never flaked while green,
			// so CI hid it — the failure message was unreliable at the one moment
			// it mattered.
			request := tc.request
			request.ID = "job-plan-bad-" + strings.ReplaceAll(name, " ", "-")
			_, err := mailbox.Enqueue(ctx, request)
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("Enqueue error = %v, want one containing %q", err, tc.wantText)
			}
			if _, getErr := store.GetJob(ctx, request.ID); getErr == nil {
				t.Fatal("rejected plan request still created a job row")
			}
		})
	}

	// The same plan aimed at omp is accepted and round-trips into the payload, so
	// a background daemon job honors it identically to a foreground one.
	job, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "job-plan-ok", Agent: "a", Action: "ask", Repo: "o/r",
		Plan: true, PlanInto: "  @smol  ",
		RuntimeOverride: runtime.OmpRuntime, RuntimeOverrideRef: "fresh:plan-seat",
	})
	if err != nil {
		t.Fatalf("Enqueue on an omp seat returned error: %v", err)
	}
	payload, err := ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if !payload.Plan || payload.PlanInto != "@smol" {
		t.Fatalf("payload plan = %v/%q, want true/%q", payload.Plan, payload.PlanInto, "@smol")
	}
	if payload.PlanMode != "" {
		t.Fatalf("payload plan_mode = %q before any dispatch, want empty (it is delivery evidence, not a request)", payload.PlanMode)
	}
}

// TestMailboxDeliverPlanGate is the authoritative dispatch gate, applied against
// the EFFECTIVE runtime. A plan request routed to a runtime with no plan mode
// FAILS the delivery and runs no subprocess at all; only an omp seat proceeds.
// Kills: dropping the flag and delivering anyway, which is the exact
// convention-only behaviour #1479 exists to remove.
func TestMailboxDeliverPlanGate(t *testing.T) {
	ctx := context.Background()
	mailbox := Mailbox{store: openTestStore(t), resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	job := db.Job{ID: "job-plan-gate", Type: "ask"}

	cases := map[string]struct {
		agentRuntime string
		payload      JobPayload
		wantText     string
	}{
		"plan on codex is refused": {
			agentRuntime: runtime.CodexRuntime,
			payload:      JobPayload{Plan: true},
			wantText:     "cannot honour plan mode (plan)",
		},
		"plan_into on claude names the resolved shape": {
			agentRuntime: runtime.ClaudeRuntime,
			payload:      JobPayload{Plan: true, PlanInto: "@smol"},
			wantText:     "cannot honour plan mode (plan-into:@smol)",
		},
		"plan_into without plan is refused even on omp": {
			agentRuntime: runtime.OmpRuntime,
			payload:      JobPayload{PlanInto: "@smol"},
			wantText:     "plan_into requires plan mode",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			adapter := &fakeDelivery{outputs: []string{planResultJSON}}
			agent := runtime.Agent{Name: "seat", Role: "implementer", Runtime: tc.agentRuntime, RepoScope: "o/r"}
			payload := tc.payload
			_, _, _, _, err := mailbox.deliver(ctx, adapter, agent, job, &payload, "work")
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("deliver error = %v, want one containing %q", err, tc.wantText)
			}
			if len(adapter.prompts) != 0 {
				t.Fatalf("refused plan request still reached the adapter: %v", adapter.prompts)
			}
			if payload.PlanMode != "" {
				t.Fatalf("refused plan request recorded evidence %q for a run that never happened", payload.PlanMode)
			}
		})
	}

	// THE BYPASS THIS GATE EXISTS TO CLOSE. readOnlyImplementationBlocked keys on
	// job type == "implement" and returns false for everything else, so before the
	// write-policy check a read-only seat refused an implement job would accept an
	// `ask` job carrying plan and write code anyway — plan mode auto-executes and
	// upstream omp adds the write tool to the active set. Every non-write policy
	// spelling is exercised because the empty and "auto" cases are the ones a seat
	// actually lands in by default, not the explicit read-only one.
	for _, policy := range []string{"", "auto", runtime.AutonomyPolicyReadOnly} {
		t.Run("a plan job is refused on a seat without write policy "+policy, func(t *testing.T) {
			adapter := &fakeDelivery{outputs: []string{planResultJSON}}
			agent := runtime.Agent{Name: "seat", Role: "implementer", Runtime: runtime.OmpRuntime, RepoScope: "o/r", AutonomyPolicy: policy}
			payload := JobPayload{Plan: true}
			_, _, _, _, err := mailbox.deliver(ctx, adapter, agent, job, &payload, "work")
			if err == nil || !strings.Contains(err.Error(), "does not grant write") {
				t.Fatalf("policy %q: deliver error = %v, want a write-policy refusal", policy, err)
			}
			if len(adapter.prompts) != 0 {
				t.Fatalf("policy %q: refused plan request still reached the adapter: %v", policy, adapter.prompts)
			}
			if payload.PlanMode != "" {
				t.Fatalf("policy %q: refused plan request recorded evidence %q", policy, payload.PlanMode)
			}
		})
	}

	t.Run("an omp seat delivers with the plan primitives intact", func(t *testing.T) {
		adapter := &fakeDelivery{outputs: []string{planResultJSON}}
		// workspace-write is REQUIRED for a plan job now, not incidental scenery: a
		// plan ends in an implementation phase, so the happy path must name the
		// policy that authorises the write it will perform.
		agent := runtime.Agent{Name: "seat", Role: "implementer", Runtime: runtime.OmpRuntime, RepoScope: "o/r", AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite}
		payload := JobPayload{Plan: true, PlanInto: "@smol"}
		if _, _, _, _, err := mailbox.deliver(ctx, adapter, agent, job, &payload, "work"); err != nil {
			t.Fatalf("deliver returned error: %v", err)
		}
		if len(adapter.plans) != 1 || !adapter.plans[0] || adapter.planIntos[0] != "@smol" {
			t.Fatalf("delivered plan primitives = %v/%v, want [true]/[@smol]", adapter.plans, adapter.planIntos)
		}
		if payload.PlanMode != "plan-into:@smol" {
			t.Fatalf("payload plan_mode = %q, want %q", payload.PlanMode, "plan-into:@smol")
		}
	})

	// Closes surviving mutant M15: gating the payload write on `err == nil` left
	// the suite green, because every plan test used a delivery that always
	// succeeds. The comment above that write exists solely to justify recording
	// evidence BEFORE the error branch — "a plan run that failed is still a plan
	// run, and that is exactly when the evidence matters" — and nothing tested it.
	// This is the same contract as the adapter-side failure returns, one layer up.
	t.Run("a failed plan delivery still records the plan evidence", func(t *testing.T) {
		adapter := &fakeDelivery{outputs: []string{planResultJSON}, err: errors.New("runtime died mid-run")}
		agent := runtime.Agent{Name: "seat", Role: "implementer", Runtime: runtime.OmpRuntime, RepoScope: "o/r", AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite}
		payload := JobPayload{Plan: true, PlanInto: "@smol"}
		if _, _, _, _, err := mailbox.deliver(ctx, adapter, agent, job, &payload, "work"); err == nil {
			t.Fatal("deliver succeeded against a failing adapter")
		}
		if payload.PlanMode != "plan-into:@smol" {
			t.Fatalf("payload plan_mode after a FAILED plan run = %q, want %q: the evidence matters most when the run died", payload.PlanMode, "plan-into:@smol")
		}
	})

	t.Run("a normal job carries no plan primitives and records no evidence", func(t *testing.T) {
		adapter := &fakeDelivery{outputs: []string{planResultJSON}}
		agent := runtime.Agent{Name: "seat", Role: "implementer", Runtime: runtime.CodexRuntime, RepoScope: "o/r"}
		payload := JobPayload{}
		if _, _, _, _, err := mailbox.deliver(ctx, adapter, agent, job, &payload, "work"); err != nil {
			t.Fatalf("deliver returned error: %v", err)
		}
		if adapter.plans[0] || adapter.planIntos[0] != "" {
			t.Fatalf("non-plan job delivered plan primitives %v/%v", adapter.plans, adapter.planIntos)
		}
		if payload.PlanMode != "" {
			t.Fatalf("non-plan job recorded plan evidence %q", payload.PlanMode)
		}
	})
}

// TestMailboxRunPersistsPlanEvidence proves the resolved plan mode survives to
// the settled payload, so a reader can tell a plan run from a normal one WITHOUT
// re-deriving it from the runtime's argv.
func TestMailboxRunPersistsPlanEvidence(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "job-plan-run", Agent: "planner", Action: "ask", Repo: "owner/repo",
		Plan: true, PlanInto: "@smol",
		RuntimeOverride: runtime.OmpRuntime, RuntimeOverrideRef: "fresh:plan-seat",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	adapter := &fakeDelivery{outputs: []string{planResultJSON}}
	agent := runtime.Agent{Name: "planner", Role: "implementer", Runtime: runtime.OmpRuntime, RuntimeRef: "fresh:plan-seat", RepoScope: "owner/repo", AutonomyPolicy: runtime.AutonomyPolicyWorkspaceWrite}
	if _, err := mailbox.Run(ctx, "job-plan-run", agent, adapter); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	stored, err := store.GetJob(ctx, "job-plan-run")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := ParseJobPayload(stored.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	if payload.PlanMode != "plan-into:@smol" {
		t.Fatalf("settled payload plan_mode = %q, want %q", payload.PlanMode, "plan-into:@smol")
	}
	if !payload.Plan || payload.PlanInto != "@smol" {
		t.Fatalf("settled payload lost the plan request: %v/%q", payload.Plan, payload.PlanInto)
	}
}
