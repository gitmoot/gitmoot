package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/github/githubtest"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

func reviewRouterHome(t *testing.T) (string, *db.Store, string) {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := config.Initialize(paths); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`
[org]
enforce = "warn"
[org.roles."owner"]
scope = ["*"]
[org.roles."joltra"]
parent = "owner"
scope = ["owner/repo"]
[review_router]
code = ["devin/swe-2", "openai-codex/gpt-5.6-sol"]
`); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	store := openCLIJobStore(t, home)
	t.Cleanup(func() { store.Close() })
	checkout, _, head := readonlyReviewWorktreeGitCheckout(t)
	seedReviewDispatchFixture(t, store, checkout)
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	previousGitHubFactory := newAgentDispatchGitHubClient
	newAgentDispatchGitHubClient = func(string) github.Client { return githubtest.NoopClient{} }
	previousPreflight := localRuntimeContractPreflight
	localRuntimeContractPreflight = func(context.Context, runtime.Agent) runtime.RuntimeContractResult {
		return runtime.RuntimeContractResult{Runtime: runtime.OmpRuntime, Version: "unknown", State: runtime.RuntimeContractUnknown, Instrument: "test"}
	}
	t.Cleanup(func() {
		newAgentDispatchGitHubClient = previousGitHubFactory
		localRuntimeContractPreflight = previousPreflight
	})
	return home, store, head
}

func runReviewRequestJSON(t *testing.T, args ...string) (reviewRequestOutput, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := runReview(append([]string{"request"}, args...), &stdout, &stderr); code != 0 {
		return reviewRequestOutput{}, fmt.Sprintf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var output reviewRequestOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	return output, ""
}

// One head, three requesters: exactly one review is dispatched, later callers
// attach, a saved verdict is reused, and a request that ends without a verdict
// does not block the next one.
func TestReviewRequestDeduplicatesOnExactHead(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	ctx := context.Background()
	seedDaemonWorkerAgentWithPolicy(t, store, "opus-reviewer", runtime.ShellRuntime, "true", []string{"review", "ask"}, "owner/repo", runtime.AutonomyPolicyReadOnly)
	seedDaemonWorkerAgentWithPolicy(t, store, "builder", runtime.ShellRuntime, "true", []string{"review", "implement"}, "owner/repo", runtime.AutonomyPolicyWorkspaceWrite)

	base := []string{"--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--role", "joltra", "--home", home, "--json"}
	first, failure := runReviewRequestJSON(t, base...)
	if failure != "" {
		t.Fatal(failure)
	}
	if first.State != reviewRequestDispatched || first.Reviewer != "opus-reviewer" || first.Model != "devin/swe-2" {
		t.Fatalf("first request = %+v, want a dispatched review on the review-only agent with the pool head", first)
	}
	if first.AwaitedFactID == 0 {
		t.Fatal("first request did not subscribe the requester to the verdict")
	}
	job, err := store.GetJob(ctx, first.JobID)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := workflow.ParseJobPayload(job.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.RuntimeOverride != runtime.OmpRuntime || payload.Model != "devin/swe-2" ||
		strings.Join(payload.ReviewModelPool, ",") != "devin/swe-2,openai-codex/gpt-5.6-sol" ||
		payload.ReviewRequester != "joltra" || !payload.NoFixTarget || payload.HeadSHA != head {
		t.Fatalf("routed payload = %+v, want omp override, pool, requester, no fix target and exact head", payload)
	}

	second, failure := runReviewRequestJSON(t, base...)
	if failure != "" {
		t.Fatal(failure)
	}
	if second.State != reviewRequestAttached || second.JobID != first.JobID {
		t.Fatalf("second request = %+v, want attachment to job %s", second, first.JobID)
	}
	jobs, err := store.ListReviewJobsForPullRequest(ctx, "owner/repo", 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("review jobs after a duplicate request = %d, want 1", len(jobs))
	}

	// A different purpose is a different question and may run in parallel.
	security, failure := runReviewRequestJSON(t, append(base, "--purpose", "security")...)
	if failure != "" {
		t.Fatal(failure)
	}
	if security.State != reviewRequestDispatched || security.JobID == first.JobID {
		t.Fatalf("security request = %+v, want its own dispatched job", security)
	}

	// The first review dies the way the daemon's dead-runtime recovery records
	// it: terminal failed WITH a synthetic `failed` result. That is not a
	// verdict, so the claim must yield to the next requester.
	deadPayload, err := workflow.ParseJobPayload(mustGetJob(t, store, first.JobID).Payload)
	if err != nil {
		t.Fatal(err)
	}
	deadPayload.Result = &workflow.AgentResult{Decision: "failed", Summary: "daemon recovery: runtime pid is dead"}
	deadEncoded, err := json.Marshal(deadPayload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionJobStatePayloadWithEvent(ctx, first.JobID, string(workflow.JobQueued), string(workflow.JobFailed), string(deadEncoded), db.JobEvent{JobID: first.JobID, Kind: "failed", Message: "runtime crashed"}); err != nil {
		t.Fatal(err)
	}
	third, failure := runReviewRequestJSON(t, base...)
	if failure != "" {
		t.Fatal(failure)
	}
	if third.State != reviewRequestDispatched || third.JobID == first.JobID {
		t.Fatalf("request after a resultless failure = %+v, want a fresh dispatch", third)
	}

	// A saved verdict is the terminal answer: attach, report it, spend nothing.
	verdictPayload, err := workflow.ParseJobPayload(mustGetJob(t, store, third.JobID).Payload)
	if err != nil {
		t.Fatal(err)
	}
	verdictPayload.Result = &workflow.AgentResult{Decision: "approved", Summary: "clean", TestsRun: []string{"go test ./..."}, Evidence: "executed"}
	encoded, err := json.Marshal(verdictPayload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionJobStatePayloadWithEvent(ctx, third.JobID, string(workflow.JobQueued), string(workflow.JobSucceeded), string(encoded), db.JobEvent{JobID: third.JobID, Kind: "succeeded", Message: "job succeeded"}); err != nil {
		t.Fatal(err)
	}
	fourth, failure := runReviewRequestJSON(t, base...)
	if failure != "" {
		t.Fatal(failure)
	}
	if fourth.State != reviewRequestVerdictExists || fourth.JobID != third.JobID || fourth.Verdict != "approved" {
		t.Fatalf("request after a saved verdict = %+v, want verdict_exists on job %s", fourth, third.JobID)
	}
	fact, err := store.GetAwaitedFact(ctx, fourth.AwaitedFactID)
	if err != nil {
		t.Fatal(err)
	}
	if fact.State != db.AwaitedFactStateSatisfied || !strings.Contains(fact.ResolutionDetail, "executed_checks=1") || !strings.Contains(fact.ResolutionDetail, "gitmoot job show "+third.JobID) {
		t.Fatalf("requester fact = %+v, want it satisfied by the saved verdict with executed-check evidence", fact)
	}
}

func mustGetJob(t *testing.T, store *db.Store, id string) db.Job {
	t.Helper()
	job, err := store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestReviewRequestReleasesClaimWhenReviewerRefused(t *testing.T) {
	home, store, head := reviewRouterHome(t)
	var stdout, stderr bytes.Buffer
	code := runReview([]string{"request", "--repo", "owner/repo", "--pr", "12", "--head", head, "--branch", "feature/review", "--role", "joltra", "--home", home, "--reviewer", "nobody"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), `reviewer "nobody"`) {
		t.Fatalf("exit=%d stderr=%q, want a refusal naming the unknown reviewer", code, stderr.String())
	}
	if _, err := store.GetReviewRequest(context.Background(), mustSubjectKey(t, head)); err == nil {
		t.Fatal("a refused request left its claim behind")
	}
}

func mustSubjectKey(t *testing.T, head string) string {
	t.Helper()
	key, err := db.ReviewRequestSubjectKey("owner/repo", 12, head, "code")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// A provider quota failure on the first pool model re-queues the review on the
// next model immediately; the same failure with the pool exhausted takes the
// ordinary timed hold. Both run through the daemon's real delivery path.
func TestReviewModelPoolFallsBackBeforeTimedHold(t *testing.T) {
	ctx := context.Background()
	store, home := blockerE2EHome(t)
	checkout := t.TempDir()
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	state := t.TempDir()
	countFile := filepath.Join(state, "count")
	replaceDiskGuardMeasurement(t, func(string) (diskFilesystemUsage, error) {
		return diskFilesystemUsage{TotalBytes: 20 << 30, FreeBytes: 10 << 30}, nil
	})
	// First delivery: the measured omp provider-stall rendering (no HTTP status).
	// Second delivery: a provider 429. Both are operational, so the first moves
	// the job to the next pool entry and the second, with the pool exhausted,
	// takes the ordinary timed hold.
	script := fmt.Sprintf(`printf x >> %q
if [ "$(wc -c < %q)" = "1" ]; then
  echo "omp turn failed (stopReason error): Provider stream stalled while waiting for the next event" 1>&2
  exit 1
fi
echo "HTTP 429 Too Many Requests: rate limit reached; try again in 3 seconds" 1>&2
exit 1`, countFile, countFile)
	seedDaemonWorkerAgent(t, store, "router-reviewer", runtime.ShellRuntime, script, []string{"review"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-pool", Agent: "router-reviewer", Action: "review", Repo: "owner/repo",
		Branch: "main", PullRequest: 1, HeadSHA: strings.Repeat("a", 40), NoFixTarget: true,
		Model: "devin/swe-2", ReviewModelPool: []string{"devin/swe-2", "openai-codex/gpt-5.6-sol"}, ReviewPurpose: "code",
	})
	worker := blockerE2EWorker(store, home, checkout)

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	job, payload := blockerE2EJobPayload(t, store, "job-pool")
	if job.State != string(workflow.JobQueued) || payload.Model != "openai-codex/gpt-5.6-sol" {
		events, _ := store.ListJobEvents(ctx, "job-pool")
		t.Fatalf("after first quota failure state=%s model=%q pool=%v events=%+v, want queued on the second pool model", job.State, payload.Model, payload.ReviewModelPool, events)
	}
	if !blockerE2EHasEventKind(t, store, "job-pool", reviewModelFallbackEventKind) {
		t.Fatalf("missing %s event", reviewModelFallbackEventKind)
	}
	retryAt, err := time.Parse(time.RFC3339Nano, payload.BlockerRetryAt)
	if err != nil || retryAt.After(time.Now().UTC()) {
		t.Fatalf("fallback retry_at = %q (%v), want an immediate re-queue", payload.BlockerRetryAt, err)
	}
	pending, err := listPendingQueuedJobs(ctx, worker, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != "job-pool" {
		t.Fatalf("pending after fallback = %+v, want the re-queued review dispatchable now", pending)
	}

	if err := runQueuedJobsForRepo(ctx, worker, 1, "", ""); err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if data, err := os.ReadFile(countFile); err != nil || strings.Count(string(data), "x") != 2 {
		t.Fatalf("deliveries = %q (%v), want two", data, err)
	}
	job, payload = blockerE2EJobPayload(t, store, "job-pool")
	if job.State != string(workflow.JobQueued) || payload.Model != "openai-codex/gpt-5.6-sol" || payload.BlockerAttempts != 2 {
		t.Fatalf("after exhausted pool state=%s model=%q attempts=%d, want the last model held for retry", job.State, payload.Model, payload.BlockerAttempts)
	}
	retryAt, err = time.Parse(time.RFC3339Nano, payload.BlockerRetryAt)
	if err != nil || !retryAt.After(time.Now().UTC()) {
		t.Fatalf("exhausted-pool retry_at = %q (%v), want the provider's timed hold", payload.BlockerRetryAt, err)
	}
}
