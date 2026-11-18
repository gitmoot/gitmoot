//go:build e2e

package cli

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// remoteProviderRetryOutcome is what one remote review left behind.
type remoteProviderRetryOutcome struct {
	job      db.Job
	events   []db.JobEvent
	attempts []db.ExecBackendAttempt
	creates  int
	deleted  []string
	started  bool
}

func (o remoteProviderRetryOutcome) eventsOfKind(kind string) []db.JobEvent {
	var matched []db.JobEvent
	for _, event := range o.events {
		if event.Kind == kind {
			matched = append(matched, event)
		}
	}
	return matched
}

func (o remoteProviderRetryOutcome) trail() string {
	var kinds []string
	for _, event := range o.events {
		kinds = append(kinds, event.Kind+": "+event.Message)
	}
	return strings.Join(kinds, "\n")
}

// runRemoteReviewAgainstProvider drives one remote E2B review through
// jobWorker.run against the fake provider planes, with the faults configure
// installs, and reports what it left.
func runRemoteReviewAgainstProvider(t *testing.T, configure func(*remoteLifecycleHarness, *db.Store)) remoteProviderRetryOutcome {
	t.Helper()
	previous := remoteProviderRetryBackoffs
	remoteProviderRetryBackoffs = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { remoteProviderRetryBackoffs = previous })

	ctx := context.Background()
	t.Setenv("HERDR_BIN", filepath.Join(t.TempDir(), "herdr-must-not-run"))
	harness := newRemoteLifecycleHarness(t)
	home, paths, store := daemonE2EHome(t)
	writeRemoteLifecycleConfig(t, paths, harness.control.URL)
	checkout := createDaemonWorkerGitCheckout(t, "main")
	makeReviewFixOriginFetchable(t, checkout, "main")
	runDaemonWorkerGit(t, checkout, "switch", "-c", "feature")
	writeReviewBaseFile(t, checkout, "under-review.txt", "review subject\n")
	runDaemonWorkerGit(t, checkout, "add", "-A")
	runDaemonWorkerGit(t, checkout, "commit", "-m", "review subject")
	head := daemonWorkerHeadSHA(t, checkout)
	serveReviewSourceFixture(t, paths, checkout, "ghs_unused2680")

	const repo = "owner/repo"
	seedDaemonWorkerRepo(t, store, repo, filepath.Join(home, "unavailable-host-checkout"))
	seedDaemonWorkerAgent(t, store, "retry-reviewer", runtime.ShellRuntime, shellApprovedResultScript, []string{"review"}, repo)
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: repo, Number: 2680, URL: "https://example.invalid/owner/repo/pull/2680",
		HeadBranch: "feature", BaseBranch: "main", HeadSHA: head, State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	remote := "remote"
	job, err := workflow.NewMailbox(store).Enqueue(ctx, workflow.JobRequest{
		ID: "provider-retry-2680", Agent: "retry-reviewer", Action: "review",
		Repo: repo, PullRequest: 2680, HeadSHA: head, Branch: "feature",
		ReviewPurpose: db.DefaultReviewPurpose, Instructions: "review it",
		ExecBackend: &remote,
	})
	if err != nil {
		t.Fatal(err)
	}
	configure(harness, store)

	worker := executionBackendJobWorker(store, io.Discard, home)
	worker.RemoteEnvdEndpointResolver = func(string, int) string { return harness.envd.URL }
	worker.ExecutionBackendFactory = worker.defaultExecutionBackend
	worker.ReviewAdmissionGitHubFactory = func(string) remoteReviewAdmissionGitHub {
		return remoteReviewAdmissionGitHubStub{
			pull:   github.PullRequest{Number: 2680, HeadSHA: head, BaseRef: "main"},
			checks: []github.PullRequestCheck{{Name: "build", Bucket: "pass"}},
		}
	}
	worker.CommenterFactory = func(string) github.Client { return &cliPollFakeGitHub{} }
	_ = worker.run(ctx, job)

	outcome := remoteProviderRetryOutcome{}
	if outcome.job, err = store.GetJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if outcome.events, err = store.ListJobEvents(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if outcome.attempts, err = store.ListExecBackendAttemptsForJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	creates, deleted, _, _, started := harness.snapshot()
	outcome.creates, outcome.deleted, outcome.started = len(creates), deleted, started
	return outcome
}

// failSyncInStart answers the first envd command of the review-source sync
// with status while sandbox n is the newest and fail(n) holds.
func failSyncInStart(harness *remoteLifecycleHarness, status int, fail func(n int) bool) {
	harness.envdFault = func(n int, call, detail string) int {
		if call == "start" && strings.HasPrefix(detail, "mkdir -p .gitmoot-source") && fail(n) {
			return status
		}
		return 0
	}
}

func requireAttemptStates(t *testing.T, outcome remoteProviderRetryOutcome, want ...string) {
	t.Helper()
	if len(outcome.attempts) != len(want) {
		t.Fatalf("ledger rows = %+v, want %d attempts in states %v\nevents:\n%s", outcome.attempts, len(want), want, outcome.trail())
	}
	for index, attempt := range outcome.attempts {
		if attempt.Attempt != index+1 || attempt.State != want[index] {
			t.Fatalf("ledger row %d = attempt %d in %s, want attempt %d in %s (rows %+v)", index, attempt.Attempt, attempt.State, index+1, want[index], outcome.attempts)
		}
	}
}

// #2680: E2B answered envd with HTTP 503 while syncing a review in, before
// the runtime started. The review retries on a fresh sandbox after tearing the
// first one down and settling its reservation, and then runs.
func TestRemoteReviewRetriesSyncIn503OnFreshSandbox(t *testing.T) {
	outcome := runRemoteReviewAgainstProvider(t, func(harness *remoteLifecycleHarness, _ *db.Store) {
		failSyncInStart(harness, http.StatusServiceUnavailable, func(n int) bool { return n == 1 })
	})
	if outcome.job.State != string(workflow.JobSucceeded) {
		t.Fatalf("state = %s, want succeeded after one retry\nevents:\n%s", outcome.job.State, outcome.trail())
	}
	retries := outcome.eventsOfKind(remoteProviderRetryEventKind)
	if len(retries) != 1 {
		t.Fatalf("%d %s events, want 1\nevents:\n%s", len(retries), remoteProviderRetryEventKind, outcome.trail())
	}
	for _, want := range []string{"attempt=1", "status=503", "phase=sync_in", "next_attempt=2"} {
		if !strings.Contains(retries[0].Message, want) {
			t.Fatalf("retry event %q does not record %q", retries[0].Message, want)
		}
	}
	if outcome.creates != 2 {
		t.Fatalf("provider saw %d creates, want 2 (the refused sandbox and its replacement)", outcome.creates)
	}
	if len(outcome.deleted) != 2 || outcome.deleted[0] != "sandbox-1" || outcome.deleted[1] != "sandbox-2" {
		t.Fatalf("deleted sandboxes = %v, want the refused sandbox-1 before the replacement sandbox-2", outcome.deleted)
	}
	// The first sandbox was destroyed and its reservation settled before the
	// second was reserved.
	requireAttemptStates(t, outcome, db.ExecBackendAttemptStateDestroyed, db.ExecBackendAttemptStateDestroyed)
	if len(outcome.eventsOfKind(remoteProviderRefusedEventKind)) != 0 {
		t.Fatal("a review that recovered was recorded as refused")
	}
}

// #2680: when every attempt gets the 503, the review fails after exactly
// three, with a message that names the likely cause.
func TestRemoteReviewFailsClearlyAfterThreeSyncIn503s(t *testing.T) {
	outcome := runRemoteReviewAgainstProvider(t, func(harness *remoteLifecycleHarness, _ *db.Store) {
		failSyncInStart(harness, http.StatusServiceUnavailable, func(int) bool { return true })
	})
	if outcome.job.State != string(workflow.JobFailed) {
		t.Fatalf("state = %s, want failed\nevents:\n%s", outcome.job.State, outcome.trail())
	}
	if outcome.creates != 3 {
		t.Fatalf("provider saw %d creates, want exactly 3 attempts", outcome.creates)
	}
	if got := len(outcome.eventsOfKind(remoteProviderRetryEventKind)); got != 2 {
		t.Fatalf("%d retry events, want 2", got)
	}
	if outcome.started {
		t.Fatal("the runtime started on a sandbox that never synced")
	}
	requireAttemptStates(t, outcome, db.ExecBackendAttemptStateDestroyed, db.ExecBackendAttemptStateDestroyed, db.ExecBackendAttemptStateDestroyed)
	failed := outcome.eventsOfKind("failed")
	if len(failed) == 0 {
		t.Fatalf("no failed event\nevents:\n%s", outcome.trail())
	}
	message := failed[len(failed)-1].Message
	for _, want := range []string{
		"E2B refused new sandboxes 3 times (HTTP 503 during sync_in).",
		"A common cause is the E2B account spending limit; check the E2B dashboard.",
		remoteLifecycleGatewayRefusal,
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("failure %q does not contain %q", message, want)
		}
	}
	if strings.Index(message, "spending limit") > strings.Index(message, remoteLifecycleGatewayRefusal) {
		t.Fatalf("failure %q puts the provider's text before the explanation", message)
	}
	if got := len(outcome.eventsOfKind(remoteProviderRefusedEventKind)); got != 1 {
		t.Fatalf("%d %s events, want 1 to count toward the cluster alert", got, remoteProviderRefusedEventKind)
	}
}

// #2680 (decision on the issue): a 503 on POST /sandboxes is NOT retried. It
// does not prove nothing was made, so the attempt's reservation stays held
// until its TTL, and against a spending limit each retry would hold another
// and fill gitmoot's own cost cap. The job fails at once with the clear
// message, which also says the reservation is held, and counts toward the
// cluster alert.
func TestRemoteReviewDoesNotRetryCreate503(t *testing.T) {
	outcome := runRemoteReviewAgainstProvider(t, func(harness *remoteLifecycleHarness, _ *db.Store) {
		harness.createFault = func(int) int { return http.StatusServiceUnavailable }
	})
	if outcome.job.State != string(workflow.JobFailed) || outcome.creates != 1 {
		t.Fatalf("state = %s after %d creates, want failed after exactly one\nevents:\n%s", outcome.job.State, outcome.creates, outcome.trail())
	}
	if got := len(outcome.eventsOfKind(remoteProviderRetryEventKind)); got != 0 {
		t.Fatalf("%d retry events for a create 503, want none", got)
	}
	requireAttemptStates(t, outcome, db.ExecBackendAttemptStateProvisioning)
	failed := outcome.eventsOfKind("failed")
	if len(failed) == 0 {
		t.Fatalf("no failed event\nevents:\n%s", outcome.trail())
	}
	message := failed[len(failed)-1].Message
	for _, want := range []string{
		"E2B refused new sandboxes once (HTTP 503 during provision).",
		"A common cause is the E2B account spending limit; check the E2B dashboard.",
		"reservation stays held until its TTL",
		"POST /sandboxes: E2B returned HTTP 503: " + remoteLifecycleGatewayRefusal,
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("failure %q does not contain %q", message, want)
		}
	}
	if got := len(outcome.eventsOfKind(remoteProviderRefusedEventKind)); got != 1 {
		t.Fatalf("%d %s events, want 1 to count toward the cluster alert", got, remoteProviderRefusedEventKind)
	}
}

// A 402 is the one status that names billing. If E2B ever sends it, the job
// says the spending limit was reached instead of a generic refusal.
func TestRemoteReviewNamesE2BSpendingLimitOn402(t *testing.T) {
	outcome := runRemoteReviewAgainstProvider(t, func(harness *remoteLifecycleHarness, _ *db.Store) {
		harness.createFault = func(int) int { return http.StatusPaymentRequired }
	})
	if outcome.job.State != string(workflow.JobFailed) || outcome.creates != 1 {
		t.Fatalf("state = %s after %d creates, want failed after exactly one\nevents:\n%s", outcome.job.State, outcome.creates, outcome.trail())
	}
	// A create 4xx proves nothing was made: the reservation is released.
	requireAttemptStates(t, outcome, db.ExecBackendAttemptStateFailed)
	failed := outcome.eventsOfKind("failed")
	if len(failed) == 0 || !strings.Contains(failed[len(failed)-1].Message, "E2B spending limit reached") {
		t.Fatalf("failure does not name the spending limit\nevents:\n%s", outcome.trail())
	}
}

// #2680 guards: a 4xx, a cost-cap refusal, and a failure once the runtime has
// started are never retried.
func TestRemoteReviewProviderRetryGuards(t *testing.T) {
	t.Run("envd 4xx during sync-in", func(t *testing.T) {
		outcome := runRemoteReviewAgainstProvider(t, func(harness *remoteLifecycleHarness, _ *db.Store) {
			failSyncInStart(harness, http.StatusBadRequest, func(int) bool { return true })
		})
		if outcome.job.State != string(workflow.JobFailed) || outcome.creates != 1 {
			t.Fatalf("state = %s after %d creates, want failed after exactly one\nevents:\n%s", outcome.job.State, outcome.creates, outcome.trail())
		}
		if got := len(outcome.eventsOfKind(remoteProviderRetryEventKind)) + len(outcome.eventsOfKind(remoteProviderRefusedEventKind)); got != 0 {
			t.Fatalf("a 400 was treated as a provider refusal (%d events)", got)
		}
		requireAttemptStates(t, outcome, db.ExecBackendAttemptStateDestroyed)
	})
	t.Run("cost cap refusal", func(t *testing.T) {
		outcome := runRemoteReviewAgainstProvider(t, func(_ *remoteLifecycleHarness, store *db.Store) {
			// Another job holds the whole cap (cost_max_reserved_usd = 25).
			if err := store.ExecForTest(context.Background(), `INSERT INTO execbackend_attempts(job_id, attempt, lifecycle_generation, provider,
				daemon_fencing_token, boot_id, ttl_expires_at, state, cost_reserved_usd)
				VALUES ('cap-holder', 1, 0, 'e2b', 'other-daemon', 'other-boot', ?, 'running', 25.0)`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
		})
		if outcome.creates != 0 {
			t.Fatalf("provider saw %d creates past a full cost cap", outcome.creates)
		}
		if got := len(outcome.eventsOfKind(remoteProviderRetryEventKind)) + len(outcome.eventsOfKind(remoteProviderRefusedEventKind)); got != 0 {
			t.Fatalf("a cost-cap refusal was treated as a provider refusal (%d events)\nevents:\n%s", got, outcome.trail())
		}
		if len(outcome.attempts) != 0 {
			t.Fatalf("a refused reservation wrote ledger rows: %+v", outcome.attempts)
		}
	})
	t.Run("503 after the runtime started", func(t *testing.T) {
		outcome := runRemoteReviewAgainstProvider(t, func(harness *remoteLifecycleHarness, _ *db.Store) {
			harness.envdFault = func(_ int, call, detail string) int {
				// The shell runtime's own command carries the agent script.
				if call == "start" && strings.Contains(detail, "shell fixture ran") {
					return http.StatusServiceUnavailable
				}
				return 0
			}
		})
		if outcome.job.State == string(workflow.JobSucceeded) || outcome.creates != 1 {
			t.Fatalf("state = %s after %d creates, want a failure on the one sandbox\nevents:\n%s", outcome.job.State, outcome.creates, outcome.trail())
		}
		if got := len(outcome.eventsOfKind(remoteProviderRetryEventKind)) + len(outcome.eventsOfKind(remoteProviderRefusedEventKind)); got != 0 {
			t.Fatalf("a runtime failure was treated as a provider refusal (%d events)", got)
		}
		requireAttemptStates(t, outcome, db.ExecBackendAttemptStateDestroyed)
	})
}
