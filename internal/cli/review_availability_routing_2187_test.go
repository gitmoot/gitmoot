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
	// EXPIRY IS ASSERTED, not merely named in the test's title (#2189 review
	// P3): the previous version checked three substrings, none of which was the
	// expiry, so a message that dropped it passed. The mutant removing the
	// expiry survived the battery.
	incident, found, err := store.GetActiveOrgRoleUnavailable(ctx(t), "joltra", time.Now().UTC())
	if err != nil || !found {
		t.Fatalf("fixture hold missing: found=%v err=%v", found, err)
	}
	for _, want := range []string{"omp", "no available runtime", "quota", strings.TrimSpace(incident.Until)} {
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

// THE CASE THE FLEET ACTUALLY RUNS (#2187, owner decision 2026-09-15).
//
// Measured before writing this: 16 of 16 reviews dispatched since the pool fix
// went live used `gitmoot agent review` with `--runtime omp` typed by hand, and
// 0 used the router. So the seat WAS choosing the runtime on every dispatch -
// the thing the router exists to prevent. A flagless dispatch must now land on
// a working runtime with nobody typing anything.
func TestAgentReviewChoosesItsRuntimeWithoutAnyFlag(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	// Registered on claude, exactly like gm-review-opus in production.
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "opus-reviewer", Action: "review",
		Instructions: "review this", Background: true, Home: home,
		PullRequest: 12, HeadSHA: head, Branch: "feature/review",
		ActingOrgRole: "joltra", NoFixTarget: true,
		// NO Runtime: the router decides.
	})
	if err != nil {
		t.Fatalf("flagless agent review: %v", err)
	}
	// UNWALLED: the named agent keeps its own runtime. Rerouting it would strip
	// the auth profile ApplyJobRuntimeOverride clears - three existing tests
	// prove that, and they broke when this preferred omp.
	if got := dispatchedRuntime(t, store, out.JobID, runtime.ClaudeRuntime); got != runtime.ClaudeRuntime {
		t.Fatalf("runtime = %q, want the named agent's own %q when nothing is walled", got, runtime.ClaudeRuntime)
	}
}

// THE CASE THAT REMOVES THE HAND-TYPED FLAG. The coordinator typed
// `--runtime omp` on all 16 post-deploy reviews because its reviewer's claude
// quota is dead - and a dead quota IS a runtime-scoped hold. With the hold
// present and no flag, the router makes that decision itself.
func TestAgentReviewReroutesOffAHeldRegisteredRuntimeWithoutAnyFlag(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	holdRole(t, store, "joltra", runtime.ClaudeRuntime)

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "opus-reviewer", Action: "review",
		Instructions: "review this", Background: true, Home: home,
		PullRequest: 15, HeadSHA: head, Branch: "feature/review",
		ActingOrgRole: "joltra", NoFixTarget: true,
	})
	if err != nil {
		t.Fatalf("flagless agent review under a claude hold: %v", err)
	}
	if got := dispatchedRuntime(t, store, out.JobID, runtime.ClaudeRuntime); got != runtime.OmpRuntime {
		t.Fatalf("runtime = %q, want %q: the seat should not have to type the escape", got, runtime.OmpRuntime)
	}
}

// And the same dispatch under a hold on the router's first choice routes to the
// agent's own runtime rather than refusing - still with no flag.
func TestAgentReviewRoutesAroundAHoldWithoutAnyFlag(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	holdRole(t, store, "joltra", runtime.OmpRuntime)

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "opus-reviewer", Action: "review",
		Instructions: "review this", Background: true, Home: home,
		PullRequest: 13, HeadSHA: head, Branch: "feature/review",
		ActingOrgRole: "joltra", NoFixTarget: true,
	})
	if err != nil {
		t.Fatalf("flagless agent review under an omp hold: %v", err)
	}
	// An omp hold is irrelevant to a named claude agent: it was never going to
	// omp. Pinned so a future reordering cannot quietly reroute it.
	if got := dispatchedRuntime(t, store, out.JobID, runtime.ClaudeRuntime); got != runtime.ClaudeRuntime {
		t.Fatalf("runtime = %q, want the agent's own %q", got, runtime.ClaudeRuntime)
	}
}

// A non-review dispatch is untouched: the router chooses runtimes for REVIEWS,
// not for every job that passes this seam.
func TestNonReviewDispatchKeepsItsOwnRuntime(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	// CLAUDE, not shell (#2187 mutation): with a shell agent the shell guard
	// short-circuits, so a mutant that reroutes EVERY action looks identical to
	// correct behaviour. The fixture has to be a runtime the router would
	// actually move.
	seedDaemonWorkerAgentWithPolicy(t, store, "builder", runtime.ClaudeRuntime, "true", []string{"ask", "implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "builder", Action: "ask",
		Instructions: "what is here", Background: true, Home: home,
		PullRequest: 12, HeadSHA: head, Branch: "feature/review", ActingOrgRole: "joltra",
	})
	if err != nil {
		t.Fatalf("ask dispatch: %v", err)
	}
	if got := dispatchedRuntime(t, store, out.JobID, runtime.ClaudeRuntime); got != runtime.ClaudeRuntime {
		t.Fatalf("non-review runtime = %q, want the agent's own %q", got, runtime.ClaudeRuntime)
	}
}

// A SHELL-registered review agent keeps its own runtime. Its runtime ref is a
// COMMAND, so routing it onto omp would run a model instead of the operator's
// script - the router choosing a runtime must not silently choose a different
// program.
func TestShellRegisteredReviewAgentIsNotRerouted(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "script-reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	out, err := dispatchLocalAgentJob(ctx, store, localAgentDispatchRequest{
		RepoFlag: "owner/repo", Agent: "script-reviewer", Action: "review",
		Instructions: "review this", Background: true, Home: home,
		PullRequest: 14, HeadSHA: head, Branch: "feature/review",
		ActingOrgRole: "joltra", NoFixTarget: true,
	})
	if err != nil {
		t.Fatalf("flagless shell review: %v", err)
	}
	if got := dispatchedRuntime(t, store, out.JobID, runtime.ShellRuntime); got != runtime.ShellRuntime {
		t.Fatalf("runtime = %q, want the script agent's own %q", got, runtime.ShellRuntime)
	}
}

// THE P1: demoting the runtime must take the MODEL with it. The pool is an
// omp provider/model chain, so a review demoted off omp that still carries the
// omp pool head runs and dies at delivery - the routing fix turning a clear
// refusal into a silent late failure.
func TestDemotedRuntimeDoesNotCarryAnOmpPoolModel(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	holdRole(t, store, "joltra", runtime.OmpRuntime)

	output, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home, "--json")
	if failure != "" {
		t.Fatal(failure)
	}
	payload := dispatchedReviewPayload(t, store, output.JobID)
	if got := dispatchedRuntime(t, store, output.JobID, runtime.ClaudeRuntime); got == runtime.OmpRuntime {
		t.Fatalf("runtime = %q: the fixture no longer demotes, so this test proves nothing", got)
	}
	if payload.Model != "" {
		t.Fatalf("demoted review carries model %q from the omp pool: it will run and die at delivery", payload.Model)
	}
}

// And on omp the pool head IS the model - so the invariant is pinned in both
// directions rather than by always clearing the field.
func TestOmpReviewStillCarriesThePoolHeadAsItsModel(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ClaudeRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)

	output, failure := runReviewRequestJSON(t, "--repo", "owner/repo", "--pr", "12", "--head", head,
		"--branch", "feature/review", "--role", "joltra", "--home", home, "--json")
	if failure != "" {
		t.Fatal(failure)
	}
	payload := dispatchedReviewPayload(t, store, output.JobID)
	if payload.Model != "devin/swe-2" {
		t.Fatalf("omp review model = %q, want the pool head", payload.Model)
	}
}
