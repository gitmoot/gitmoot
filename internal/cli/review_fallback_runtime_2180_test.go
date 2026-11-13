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
// running review, without a delivery.
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
		PullRequest: 1, HeadSHA: strings.Repeat("d", 40),
		ReviewPurpose: "code", ReviewModelPool: []string{"sentinel/router-a", "sentinel/router-b"},
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
	stall := runBlockerOnRunningReview(t, runtime.OmpRuntime,
		poolReviewRequest("job-stall", "omp-reviewer"),
		errors.New("omp turn failed (stopReason error): Provider stream stalled while waiting for the next event"))
	if stall.Model != "sentinel/router-a" {
		t.Fatalf("provider stall: model=%q, want the pool head", stall.Model)
	}

	generic := runBlockerOnRunningReview(t, runtime.OmpRuntime,
		poolReviewRequest("job-net", "omp-reviewer"),
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
	mailbox := workflow.NewMailbox(store)
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
