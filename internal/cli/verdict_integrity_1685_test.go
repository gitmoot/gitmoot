package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// seedVerdictIntegrityAgent registers one agent with an explicit role so the
// role/capability split #1685 turns on is directly expressible.
func seedVerdictIntegrityAgent(t *testing.T, store *db.Store, name, role string, capabilities []string) db.Agent {
	t.Helper()
	agent := db.Agent{
		Name: name, Role: role, Runtime: runtime.ShellRuntime, RuntimeRef: "true",
		RepoScope: "owner/repo", Capabilities: capabilities,
		AutonomyPolicy: runtime.AutonomyPolicyAuto, HealthStatus: "ok",
	}
	if err := store.UpsertAgent(context.Background(), agent); err != nil {
		t.Fatalf("UpsertAgent(%s): %v", name, err)
	}
	return agent
}

// #1685. A coordinator in a review slot is the review-panel recipe, so dispatch
// must allow it. An earlier version of this file pinned the opposite - it
// refused role=coordinator outright - which made the product refuse its own
// documented recipe (skills/gitmoot/agent-templates/review-panel.md declares
// capabilities [ask, review]). Reviews can no longer return delegations (#2588),
// so a coordinator's review result is judged like any other verdict.
func TestEnsureLocalAgentAccessAllowsCoordinatorInReviewSlot(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	for i, role := range []string{
		"coordinator", "Coordinator", "review-coordinator", "coordinator-sol", "panel-coordinator",
	} {
		agent := seedVerdictIntegrityAgent(t, store, fmt.Sprintf("coord-%d", i), role, []string{"ask", "review"})
		agent.Role = role
		if err := ensureLocalAgentAccess(ctx, store, agent, "owner/repo", "review"); err != nil {
			t.Fatalf("role %q must be dispatchable as a reviewer, got %v", role, err)
		}
	}
}

// ACCEPTANCE: the actual reviewer for this repo must be unaffected.
func TestEnsureLocalAgentAccessAllowsReviewerRoleInReviewSlot(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	for _, role := range []string{"reviewer", "agent", "lead", "implementer", "planner", ""} {
		reviewer := seedVerdictIntegrityAgent(t, store, "reviewer-"+role+"x", role, []string{"review"})
		if err := ensureLocalAgentAccess(ctx, store, reviewer, "owner/repo", "review"); err != nil {
			t.Fatalf("role %q must be allowed to review, got %v", role, err)
		}
	}
}

// The capability guard must still fire on its own axis. Role and capability are
// two different questions and neither subsumes the other.
func TestEnsureLocalAgentAccessStillEnforcesReviewCapability(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	noReview := seedVerdictIntegrityAgent(t, store, "impl-only", "implementer", []string{"implement"})
	err := ensureLocalAgentAccess(ctx, store, noReview, "owner/repo", "review")
	if err == nil || !strings.Contains(err.Error(), "lacks review capability") {
		t.Fatalf("capability guard error = %v, want lacks review capability", err)
	}
}
