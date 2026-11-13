package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
)

// #1250. Native review fanout children were enqueued with NO org attribution —
// measured at 0 of 99 workflow-* jobs on the live store, a total gap rather than
// a thin one. Attribution is what event_sink/event_rule_sink use as the WAKE
// TARGET ROLE, so an unattributed job's blocked event has no owner to wake: the
// same machinery behind the dropped wake rows of #1347. #1250 and #1347 are one
// defect seen from two ends.
//
// The in-process PR-open trigger takes the role from the implement job's own
// payload (#2591 retired the branch lock that used to carry it).
func TestInProcessPROpenAttributesFanoutChildrenFromImplementPayload(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "reviewer", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"reviewer"}

	insertCompletedJob(t, store, db.Job{
		ID:    "attributed-implement",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:          "gitmoot/gitmoot",
		Branch:        "task-20",
		PullRequest:   20,
		HeadSHA:       "head020",
		TaskID:        "task-20",
		LeadAgent:     "lead",
		ActingOrgRole: "gmc-fanout",
		Result:        &AgentResult{Decision: "implemented", Summary: "opened PR"},
	})

	if err := engine.AdvanceJob(ctx, "attributed-implement"); err != nil {
		t.Fatalf("AdvanceJob returned error: %v", err)
	}

	assertFanoutChildRole(t, ctx, store, "gmc-fanout")
}

// Legacy polarity, mandatory per ruling 20630 condition (1). An implement job
// with no recorded role must degrade to today's behaviour — the fanout still
// runs, the child is simply unattributed, and nothing invents an attribution.
func TestInProcessPROpenLeavesFanoutChildrenUnattributedWithoutRole(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "reviewer", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RequiredReviewers = []string{"reviewer"}

	insertCompletedJob(t, store, db.Job{
		ID:    "legacy-implement",
		Agent: "lead",
		Type:  "implement",
	}, JobPayload{
		Repo:        "gitmoot/gitmoot",
		Branch:      "task-21",
		PullRequest: 21,
		HeadSHA:     "head021",
		TaskID:      "task-21",
		LeadAgent:   "lead",
		Result:      &AgentResult{Decision: "implemented", Summary: "opened PR"},
	})

	if err := engine.AdvanceJob(ctx, "legacy-implement"); err != nil {
		t.Fatalf("missing role broke the advance instead of degrading: %v", err)
	}

	// The fanout still happens — behaviour is unchanged, only attribution is absent.
	assertFanoutChildRole(t, ctx, store, "")
}

func assertFanoutChildRole(t *testing.T, ctx context.Context, store *db.Store, want string) {
	t.Helper()
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	found := false
	for _, job := range jobs {
		if job.Type != "review" {
			continue
		}
		found = true
		payload, err := ParseJobPayload(job.Payload)
		if err != nil {
			t.Fatalf("ParseJobPayload returned error: %v", err)
		}
		if payload.ActingOrgRole != want {
			t.Fatalf("fanout child acting_org_role = %q, want %q; an unattributed fanout child has no owner to wake (#1347): %+v", payload.ActingOrgRole, want, payload)
		}
	}
	if !found {
		t.Fatal("no review job was enqueued; cannot assert attribution")
	}
}

// #1250 finding 2 (g7-review). The risk-tiered path diverts BEFORE the ordinary
// attributed request loop, so the whole high-risk branch was unattributed.
func TestHighRiskFanoutAttributesLensChildren(t *testing.T) {
	ctx := context.Background()
	store := openEngineStore(t)
	seedAgent(t, store, "lead", []string{"implement"}, "gitmoot/gitmoot")
	seedAgent(t, store, "audit", []string{"review"}, "gitmoot/gitmoot")
	seedAgent(t, store, "sec", []string{"review"}, "gitmoot/gitmoot")
	engine := testEngine(store)
	engine.RiskTiersEnabled = true

	event := highRiskEvent()
	event.ActingOrgRole = "gmc-fanout"
	if err := engine.HandlePullRequestOpened(ctx, event); err != nil {
		t.Fatalf("HandlePullRequestOpened returned error: %v", err)
	}

	jobs, err := store.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs returned error: %v", err)
	}
	lenses := 0
	for _, job := range jobs {
		if !strings.Contains(job.ID, "/delegation/") {
			continue
		}
		lenses++
		payload, err := ParseJobPayload(job.Payload)
		if err != nil {
			t.Fatalf("ParseJobPayload returned error: %v", err)
		}
		if payload.ActingOrgRole != "gmc-fanout" {
			t.Fatalf("high-risk lens %q acting_org_role = %q, want gmc-fanout", job.ID, payload.ActingOrgRole)
		}
	}
	if lenses == 0 {
		t.Fatal("no lens children were enqueued; cannot assert attribution")
	}
}
