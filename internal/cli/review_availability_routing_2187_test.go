package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// #2187. `gitmoot review request` exists so a requester names a pull request
// and decides nothing else - the router picks the reviewer, the runtime and the
// model. That promise broke in one direction: selection pinned omp without
// consulting availability, and the hold was tested at DISPATCH, where the only
// remaining move is to refuse.
//
// Measured cost on 2026-09-14: two seats spent three dispatch attempts and two
// escalations discovering that a review they were obliged to obtain could not
// be dispatched, and the remedy was to type `--runtime omp` by hand.
func holdRole(t *testing.T, store *db.Store, role, runtimeName string) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.UpsertOrgRoleUnavailableForRuntime(ctx(t), role, runtimeName, "quota", now.Add(6*time.Hour), now); err != nil {
		t.Fatalf("UpsertOrgRoleUnavailableForRuntime: %v", err)
	}
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}

// The headline: a held role still gets a dispatched review, with no flag.
func TestReviewRequestRoutesAroundAHeldRuntime(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	// The reviewer is registered on claude, so the router's omp pin is not the
	// agent's own runtime - exactly the shape the fleet runs.
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	holdRole(t, store, "joltra", runtime.OmpRuntime)

	output, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home, "--json")
	if failure != "" {
		t.Fatalf("a role held on omp could not obtain a review without a flag: %s", failure)
	}
	if output.State != reviewRequestDispatched {
		t.Fatalf("state = %q, want dispatched", output.State)
	}
	payload := dispatchedReviewPayload(t, store, output.JobID)
	if payload.RuntimeOverride == runtime.OmpRuntime {
		t.Fatalf("dispatched onto the held runtime %q anyway", payload.RuntimeOverride)
	}
	if got := dispatchedRuntime(t, store, output.JobID, runtime.ClaudeRuntime); got != runtime.ClaudeRuntime {
		t.Fatalf("runtime = %q, want the candidate's own %q once omp is walled", got, runtime.ClaudeRuntime)
	}
}

// The unheld case must be untouched: the omp pin still wins, so routing only
// engages when there is something to route around.
func TestReviewRequestKeepsTheOmpPinWhenNothingIsHeld(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	output, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home, "--json")
	if failure != "" {
		t.Fatal(failure)
	}
	if got := dispatchedRuntime(t, store, output.JobID, runtime.ClaudeRuntime); got != runtime.OmpRuntime {
		t.Fatalf("runtime = %q, want the router's omp pin when no hold exists", got)
	}
}

// When every option is walled the refusal must NAME the scope and expiry. The
// refusal two seats hit named neither, which is why they escalated instead of
// routing elsewhere.
func TestReviewRequestRefusalNamesTheHeldRuntimeAndExpiry(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	// Only an omp-registered reviewer exists, so the omp hold walls every option.
	seedDaemonWorkerAgentWithPolicy(t, store, "omp-reviewer", runtime.OmpRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	holdRole(t, store, "joltra", runtime.OmpRuntime)

	_, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home, "--json")
	if failure == "" {
		t.Fatal("a fully walled role obtained a review")
	}
	for _, want := range []string{"omp", "no available runtime", "quota"} {
		if !strings.Contains(failure, want) {
			t.Fatalf("refusal = %q, want it to name %q", failure, want)
		}
	}
}

// An explicit operator runtime still wins: the router routes, it does not
// overrule someone who stated a runtime deliberately.
func TestExplicitRuntimeStillOverridesRouting(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	// NO HOLD, deliberately: with one, routing would pick claude by itself and
	// the assertion could not tell an honoured override from a coincidence -
	// the equality-by-coincidence shape this campaign keeps producing. Unheld,
	// routing picks omp, so only a real override yields claude.
	output, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home,
		"--runtime", runtime.ClaudeRuntime, "--json")
	if failure != "" {
		t.Fatal(failure)
	}
	if got := dispatchedRuntime(t, store, output.JobID, runtime.ClaudeRuntime); got != runtime.ClaudeRuntime {
		t.Fatalf("runtime = %q, want the operator's explicit choice", got)
	}
}

// A named --reviewer must be routed too. Naming the AGENT is not naming the
// RUNTIME: the requester has still decided nothing about which runtime carries
// the review, so the hold applies exactly as it does to router-chosen agents.
// Mutation showed this path uncovered while the router path was pinned.
func TestExplicitReviewerIsRoutedAroundAHeldRuntimeToo(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	holdRole(t, store, "joltra", runtime.OmpRuntime)

	output, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home,
		"--reviewer", "opus-reviewer", "--json")
	if failure != "" {
		t.Fatalf("a named reviewer could not be dispatched under an omp hold: %s", failure)
	}
	if got := dispatchedRuntime(t, store, output.JobID, runtime.ClaudeRuntime); got != runtime.ClaudeRuntime {
		t.Fatalf("runtime = %q, want the named reviewer's own %q once omp is walled", got, runtime.ClaudeRuntime)
	}
}

// And a named reviewer with no available runtime refuses with the same named
// scope, rather than being dispatched onto the walled one.
func TestExplicitReviewerWithNoAvailableRuntimeRefusesByName(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "omp-reviewer", runtime.OmpRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	holdRole(t, store, "joltra", runtime.OmpRuntime)

	_, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home,
		"--reviewer", "omp-reviewer", "--json")
	if failure == "" {
		t.Fatal("a named reviewer with no available runtime was dispatched anyway")
	}
	if !strings.Contains(failure, "no available runtime") || !strings.Contains(failure, "omp") {
		t.Fatalf("refusal = %q, want the named scope", failure)
	}
}
