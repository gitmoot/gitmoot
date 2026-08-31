package cli

import (
	"context"
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

// #1685 layer 4. g6-review-sol is registered role=coordinator with
// caps=ask,review. Every dispatcher that picked it by name got a fan-out where
// it expected a verdict, because the dispatch path gates on CAPABILITY and
// nothing reads ROLE. The name reading "*-review-*" is what made it survive:
// the convention held for 30 agents and failed silently on the 31st.
func TestEnsureLocalAgentAccessRefusesCoordinatorInReviewSlot(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	coordinator := seedVerdictIntegrityAgent(t, store, "g6-review-sol", "coordinator", []string{"ask", "review"})

	err := ensureLocalAgentAccess(ctx, store, coordinator, "owner/repo", "review")
	if err == nil {
		t.Fatal("a coordinator-role agent must be refused in a review slot")
	}
	for _, want := range []string{"g6-review-sol", "coordinator", "not a verdict"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q must name %q", err, want)
		}
	}

	// DISTINCTNESS: the refusal is on ROLE, not capability. The same agent is
	// perfectly valid in the slot it was built for, so this guard must not
	// degenerate into "coordinators cannot be dispatched".
	if err := ensureLocalAgentAccess(ctx, store, coordinator, "owner/repo", "ask"); err != nil {
		t.Fatalf("a coordinator must still be dispatchable as an ask, got %v", err)
	}
}

// ACCEPTANCE: the actual reviewer for this repo must be unaffected. A guard that
// also refuses g7-review has replaced a rare vacuous verdict with a total outage.
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

// The role comparison is case-insensitive because the registry column is a
// free-form string: "Coordinator" must not walk through the gate that
// "coordinator" is refused by.
func TestEnsureLocalAgentAccessCoordinatorRoleIsCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	for _, role := range []string{"Coordinator", "COORDINATOR", "  coordinator  "} {
		agent := seedVerdictIntegrityAgent(t, store, "coord-"+strings.TrimSpace(strings.ToLower(role))+"-x", role, []string{"review"})
		agent.Role = role
		if err := ensureLocalAgentAccess(ctx, store, agent, "owner/repo", "review"); err == nil {
			t.Fatalf("role %q must be refused in a review slot", role)
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
