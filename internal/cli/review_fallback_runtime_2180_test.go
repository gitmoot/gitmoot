package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// quotaFailure is the provider rendering the classifier recognises as a quota
// blocker; the e2e fixtures emit the same text from a shell script.
const quotaFailure = "HTTP 429 Too Many Requests: rate limit reached; try again in 3 seconds"

// runBlockerOnRunningReview drives the REAL pre-terminal blocker seam against a
// running review, without a delivery: the runtime switch under test happens
// here, and exercising it through a live claude or codex delivery is impossible
// on this box while the only authenticated Codex home is quota-capped.
func runBlockerOnRunningReview(t *testing.T, agentRuntime string, request workflow.JobRequest, cause error) workflow.JobPayload {
	t.Helper()
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, request.Agent, agentRuntime, "true", []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, request)
	if _, err := store.TransitionJobState(ctx, request.ID, string(workflow.JobQueued), string(workflow.JobRunning)); err != nil {
		t.Fatalf("TransitionJobState: %v", err)
	}
	worker := blockerE2EWorker(store, home, checkout)
	// Production reaches this seam from the DELIVERY seam, and the classifier
	// deliberately refuses an unwrapped error - a bare string must not be able
	// to claim a provider blocker. Wrapping is what makes this fixture the real
	// path rather than a friendlier one.
	if _, err := worker.deferOperationalBlockerPreTerminal(ctx, request.ID, workflow.DeliveryError{Err: cause}); err != nil {
		t.Fatalf("deferOperationalBlockerPreTerminal: %v", err)
	}
	_, payload := blockerE2EJobPayload(t, store, request.ID)
	return payload
}

func poolReviewRequest(id, agent string) workflow.JobRequest {
	return workflow.JobRequest{
		ID: id, Agent: agent, Action: "review", Repo: "owner/repo", Branch: "main",
		PullRequest: 1, HeadSHA: strings.Repeat("d", 40), NoFixTarget: true,
		ReviewPurpose: "code", ReviewModelPool: []string{"sentinel/router-a", "sentinel/router-b"},
	}
}

// The owner's fallback decision, exercised rather than inspected (#2186 round 3
// led with this): a quota-blocked review on a MODEL-DRIVEN runtime retries on
// omp with the next pool model. Without the switch the retry would emit an
// omp-qualified model name on a runtime that cannot resolve it.
func TestQuotaBlockerMovesAClaudeReviewToOmpWithThePoolHead(t *testing.T) {
	payload := runBlockerOnRunningReview(t, runtime.ClaudeRuntime,
		poolReviewRequest("job-claude", "claude-reviewer"), errors.New(quotaFailure))

	if payload.Model != "sentinel/router-a" {
		t.Fatalf("model = %q, want the first untried pool entry", payload.Model)
	}
	if payload.RuntimeOverride != runtime.OmpRuntime {
		t.Fatalf("runtime_override = %q, want %q: the retry would otherwise hand an omp model to claude",
			payload.RuntimeOverride, runtime.OmpRuntime)
	}
	if !strings.HasPrefix(payload.RuntimeOverrideRef, runtime.FreshRefPrefix) {
		t.Fatalf("runtime_override_ref = %q, want a fresh session: an empty ref resumes an unspecified session", payload.RuntimeOverrideRef)
	}
}

// The auth-profile question the reviewer led with, answered by execution rather
// than by inspection. ApplyJobRuntimeOverride clears the agent's model/effort
// and switches its config dir - the stripping that made runtime PARITY wrong at
// dispatch. It is correct HERE and the difference is the precondition: at
// dispatch the claude credential was fine and the override destroyed it; at
// fallback that credential has ALREADY failed, and the job's own pool model
// replaces the cleared one in the same write.
func TestFallbackOverrideLeavesAnExecutableOmpAgent(t *testing.T) {
	payload := runBlockerOnRunningReview(t, runtime.ClaudeRuntime,
		poolReviewRequest("job-profile", "claude-reviewer"), errors.New(quotaFailure))

	effective := applyJobRuntimeOverride(runtime.Agent{
		Name: "claude-reviewer", Runtime: runtime.ClaudeRuntime,
		Model: "anthropic/claude-opus-5", Effort: "high",
	}, payload)

	if effective.Runtime != runtime.OmpRuntime {
		t.Fatalf("effective runtime = %q, want omp", effective.Runtime)
	}
	// The agent-level model is cleared by the override; the job carries the
	// replacement. A cleared agent model with no job model would dispatch a
	// review with no model at all, which is the failure this pins.
	if effective.Model != "" {
		t.Fatalf("agent model = %q, want cleared by the override", effective.Model)
	}
	if payload.Model == "" {
		t.Fatal("job carries no model after the override cleared the agent's: the retry has nothing to run")
	}
}

// A script agent must NOT be switched: on the shell runtime the agent's command
// is what executes and the model names nothing, so moving it to omp would
// silently replace the operator's script with a model.
func TestQuotaBlockerKeepsAScriptAgentOnItsRuntime(t *testing.T) {
	payload := runBlockerOnRunningReview(t, runtime.ShellRuntime,
		poolReviewRequest("job-script-agent", "script-reviewer"), errors.New(quotaFailure))

	if payload.Model != "sentinel/router-a" {
		t.Fatalf("model = %q, want the pool head", payload.Model)
	}
	if payload.RuntimeOverride != "" {
		t.Fatalf("runtime_override = %q, want none for a script agent", payload.RuntimeOverride)
	}
}

// The omp-transport guard: a network outage advances the pool ONLY when the
// failure is an omp provider transport stall. A plain network error is not a
// fact about one provider, so a different model cannot fix it.
func TestNetworkOutageAdvancesThePoolOnlyForAProviderStall(t *testing.T) {
	stall := runBlockerOnRunningReview(t, runtime.ClaudeRuntime,
		poolReviewRequest("job-stall", "claude-reviewer"),
		errors.New("omp turn failed (stopReason error): Provider stream stalled while waiting for the next event"))
	if stall.Model != "sentinel/router-a" || stall.RuntimeOverride != runtime.OmpRuntime {
		t.Fatalf("provider stall: model=%q runtime=%q, want the pool head on omp", stall.Model, stall.RuntimeOverride)
	}

	generic := runBlockerOnRunningReview(t, runtime.ClaudeRuntime,
		poolReviewRequest("job-net", "claude-reviewer"),
		errors.New("dial tcp: lookup api.github.com: i/o timeout"))
	if generic.Model != "" || generic.RuntimeOverride != "" {
		t.Fatalf("generic network outage advanced the pool: model=%q runtime=%q", generic.Model, generic.RuntimeOverride)
	}
}

// #2186 F3: the fail-open degradation must be visible. A review that enqueues
// with no resolvable pool has no provider fallback, which is indistinguishable
// from an armed pool unless the job says so.
func TestPoollessReviewCarriesTheUnresolvedAdvisory(t *testing.T) {
	ctx := context.Background()
	store, _ := blockerE2EHome(t)
	seedDaemonWorkerRepo(t, store, "owner/repo", t.TempDir())
	seedDaemonWorkerAgent(t, store, "reviewer", runtime.ShellRuntime, "true", []string{"review"}, "owner/repo")
	mailbox := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("test"))
	mailbox.ReviewModelPool = func(string) []string { return nil }
	if _, err := mailbox.Enqueue(ctx, workflow.JobRequest{
		ID: "job-poolless", Agent: "reviewer", Action: "review", Repo: "owner/repo",
		Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("e", 40), ReviewPurpose: "code",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	events, err := store.ListJobEvents(ctx, "job-poolless")
	if err != nil {
		t.Fatal(err)
	}
	var found db.JobEvent
	for _, event := range events {
		if event.Kind == "review_pool_unresolved" {
			found = event
		}
	}
	if found.Kind == "" {
		t.Fatal("no review_pool_unresolved advisory: a pool-less review is silently without a fallback")
	}
	if !strings.Contains(found.Message, `"code"`) {
		t.Fatalf("advisory = %q, want it to name the purpose that resolved nothing", found.Message)
	}
}
