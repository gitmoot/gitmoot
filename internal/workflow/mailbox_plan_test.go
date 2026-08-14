package workflow

import (
	"context"
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
	mailbox := Mailbox{Store: store}

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
			_, err := mailbox.Enqueue(ctx, tc.request)
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("Enqueue error = %v, want one containing %q", err, tc.wantText)
			}
			if _, getErr := store.GetJob(ctx, tc.request.ID); getErr == nil {
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
	mailbox := Mailbox{Store: openTestStore(t)}
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

	t.Run("an omp seat delivers with the plan primitives intact", func(t *testing.T) {
		adapter := &fakeDelivery{outputs: []string{planResultJSON}}
		agent := runtime.Agent{Name: "seat", Role: "implementer", Runtime: runtime.OmpRuntime, RepoScope: "o/r"}
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
	mailbox := Mailbox{Store: store}
	if _, err := mailbox.Enqueue(ctx, JobRequest{
		ID: "job-plan-run", Agent: "planner", Action: "ask", Repo: "owner/repo",
		Plan: true, PlanInto: "@smol",
		RuntimeOverride: runtime.OmpRuntime, RuntimeOverrideRef: "fresh:plan-seat",
	}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	adapter := &fakeDelivery{outputs: []string{planResultJSON}}
	agent := runtime.Agent{Name: "planner", Role: "implementer", Runtime: runtime.OmpRuntime, RuntimeRef: "fresh:plan-seat", RepoScope: "owner/repo"}
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
