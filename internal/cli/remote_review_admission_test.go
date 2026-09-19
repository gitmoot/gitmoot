//go:build e2e

package cli

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/execbackend/e2b"
	"github.com/gitmoot/gitmoot/internal/github"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

type remoteReviewAdmissionGitHubStub struct {
	pull      github.PullRequest
	pullErr   error
	checks    []github.PullRequestCheck
	checksErr error
}

func (s remoteReviewAdmissionGitHubStub) GetPullRequest(context.Context, github.Repository, int64) (github.PullRequest, error) {
	return s.pull, s.pullErr
}

func (s remoteReviewAdmissionGitHubStub) ListPullRequestChecks(context.Context, github.Repository, int64) ([]github.PullRequestCheck, error) {
	return s.checks, s.checksErr
}

type remoteReviewProbeBackend struct {
	provisionCalls int
	provisionErr   error
}

func (b *remoteReviewProbeBackend) Name() execbackend.Backend { return execbackend.Remote }
func (b *remoteReviewProbeBackend) Provision(context.Context, execbackend.JobScope) (*execbackend.Instance, error) {
	b.provisionCalls++
	return nil, b.provisionErr
}
func (*remoteReviewProbeBackend) Attach(context.Context, string) (*execbackend.Instance, error) {
	return nil, errors.New("unexpected Attach")
}
func (*remoteReviewProbeBackend) SyncIn(context.Context, *execbackend.Instance, execbackend.Materials) error {
	return errors.New("unexpected SyncIn")
}
func (*remoteReviewProbeBackend) Exec(context.Context, *execbackend.Instance, execbackend.Command) (execbackend.Stream, error) {
	return nil, errors.New("unexpected Exec")
}
func (*remoteReviewProbeBackend) Collect(context.Context, *execbackend.Instance) (execbackend.ChangeSet, error) {
	return execbackend.ChangeSet{}, errors.New("unexpected Collect")
}
func (*remoteReviewProbeBackend) Cancel(context.Context, *execbackend.Instance) error  { return nil }
func (*remoteReviewProbeBackend) Destroy(context.Context, *execbackend.Instance) error { return nil }

type remoteReviewAdmissionFixture struct {
	ctx                        context.Context
	home                       string
	paths                      config.Paths
	store                      *db.Store
	job                        db.Job
	head                       string
	worker                     jobWorker
	github                     *remoteReviewAdmissionGitHubStub
	backend                    *remoteReviewProbeBackend
	factoryCalls               int
	commentCalls               int
	beforeBackendFactoryReturn func()
}

func newRemoteReviewAdmissionFixture(t *testing.T, runtimeName string) *remoteReviewAdmissionFixture {
	t.Helper()
	ctx := context.Background()
	remoteBackend := string(execbackend.Remote)
	home, paths, store := heartbeatLoopE2EHome(t)
	writeRemoteLifecycleConfig(t, paths, "")
	checkout := createDaemonWorkerGitCheckout(t, "remote-review-admission")
	head := daemonWorkerHeadSHA(t, checkout)
	makeReviewFixOriginFetchable(t, checkout, "remote-review-admission")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	seedDaemonWorkerAgent(t, store, "remote-review-agent", runtimeName, heartbeatShellResultScript, []string{"review"}, "owner/repo")
	if err := store.UpsertPullRequest(ctx, db.PullRequest{
		RepoFullName: "owner/repo", Number: 2238, URL: "https://example.invalid/owner/repo/pull/2238",
		HeadBranch: "remote-review-admission", BaseBranch: "remote-review-admission", HeadSHA: head, State: "open",
	}); err != nil {
		t.Fatal(err)
	}
	job, err := workflow.NewMailbox(store, workflow.UnavailableDeliveryWorktreeResolver("worker owns checkout")).Enqueue(ctx, workflow.JobRequest{
		ID: "remote-review-admission", Agent: "remote-review-agent", Action: "review",
		Repo: "owner/repo", PullRequest: 2238, HeadSHA: head, Branch: "remote-review-admission",
		ReviewPurpose: db.DefaultReviewPurpose, Instructions: "review the exact head",
		ExecBackend: &remoteBackend,
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := &remoteReviewAdmissionGitHubStub{pull: github.PullRequest{Number: 2238, HeadSHA: head}}
	probe := &remoteReviewProbeBackend{provisionErr: errors.New("provider probe stops after Provision")}
	fixture := &remoteReviewAdmissionFixture{ctx: ctx, home: home, paths: paths, store: store, job: job, head: head, github: stub, backend: probe}
	fixture.worker = defaultJobWorker(store, io.Discard, home)
	fixture.worker.ReviewAdmissionGitHubFactory = func(string) remoteReviewAdmissionGitHub { return fixture.github }
	fixture.worker.ExecutionBackendFactory = func(_ execbackend.Backend, _ config.RemoteExecConfig) (execbackend.ExecutionBackend, error) {
		fixture.factoryCalls++
		if fixture.beforeBackendFactoryReturn != nil {
			fixture.beforeBackendFactoryReturn()
		}
		return fixture.backend, nil
	}
	fixture.worker.CommenterFactory = func(string) github.Client {
		fixture.commentCalls++
		return &cliPollFakeGitHub{}
	}
	return fixture
}

func (f *remoteReviewAdmissionFixture) run(t *testing.T) {
	t.Helper()
	if err := f.worker.run(f.ctx, f.job); err != nil {
		t.Fatalf("worker.run returned error: %v", err)
	}
}

func (f *remoteReviewAdmissionFixture) assertRefusedBeforeProvision(t *testing.T, reason string, preexistingAttempts int) {
	t.Helper()
	if f.factoryCalls != 0 || f.backend.provisionCalls != 0 {
		t.Fatalf("backend factory/provider calls = %d/%d, want 0/0", f.factoryCalls, f.backend.provisionCalls)
	}
	if f.commentCalls != 0 {
		t.Fatalf("commenter factory calls = %d, want 0", f.commentCalls)
	}
	attempts, err := f.store.ListExecBackendAttemptsForJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != preexistingAttempts {
		t.Fatalf("attempt ledger rows = %d, want %d", len(attempts), preexistingAttempts)
	}
	events, err := f.store.ListJobEvents(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantKind := "remote_review_admission_avoided_" + reason
	for _, event := range events {
		if event.Kind == wantKind {
			return
		}
	}
	t.Fatalf("events = %+v, want durable counter %q", events, wantKind)
}

// This exercises the production dispatch payload through jobWorker.run and the
// exact boundary immediately before the ledger reservation/provider factory.
// Each refused case must leave both observable side-effect counts at zero.
func TestRemoteReviewAdmissionRefusesBeforeReservationAndProvider(t *testing.T) {
	t.Run("stale", func(t *testing.T) {
		f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
		f.github.pull.HeadSHA = strings.Repeat("a", 40)
		f.run(t)
		f.assertRefusedBeforeProvision(t, remoteReviewAvoidedStale, 0)
	})

	t.Run("duplicate", func(t *testing.T) {
		f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
		owner, err := workflow.NewMailbox(f.store, workflow.UnavailableDeliveryWorktreeResolver("not run")).Enqueue(f.ctx, workflow.JobRequest{
			ID: "remote-review-owner", Agent: "remote-review-agent", Action: "review",
			Repo: "owner/repo", PullRequest: 2238, HeadSHA: f.head, Branch: "remote-review-admission",
			ReviewPurpose: db.DefaultReviewPurpose, Instructions: "owns the subject",
		})
		if err != nil {
			t.Fatal(err)
		}
		key, err := db.ReviewRequestSubjectKey("owner/repo", 2238, f.head, db.DefaultReviewPurpose)
		if err != nil {
			t.Fatal(err)
		}
		if _, won, err := f.store.ClaimReviewRequest(f.ctx, key, owner.ID, db.DefaultReviewPurpose, "test", reviewRequestOwner()); err != nil || !won {
			t.Fatalf("seed subject claim: won=%v err=%v", won, err)
		}
		f.run(t)
		f.assertRefusedBeforeProvision(t, remoteReviewAvoidedDuplicate, 0)
	})

	t.Run("unsupported", func(t *testing.T) {
		f := newRemoteReviewAdmissionFixture(t, runtime.ClaudeRuntime)
		f.run(t)
		f.assertRefusedBeforeProvision(t, remoteReviewAvoidedUnsupported, 0)
	})

	t.Run("red CI", func(t *testing.T) {
		f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
		file, err := os.OpenFile(f.paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := io.WriteString(file, "\n[review]\nremote_require_ci_green = true\n")
		if err := errors.Join(writeErr, file.Close()); err != nil {
			t.Fatal(err)
		}
		f.github.checks = []github.PullRequestCheck{{Name: "build", State: "failure", Bucket: "fail"}}
		f.run(t)
		f.assertRefusedBeforeProvision(t, remoteReviewAvoidedRedCI, 0)
	})

	t.Run("unclassified retry", func(t *testing.T) {
		f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
		if err := f.store.ReserveExecBackendAttempt(f.ctx, db.ExecBackendAttemptReservation{
			ExecBackendAttemptKey: db.ExecBackendAttemptKey{JobID: f.job.ID, Attempt: 1, LifecycleGeneration: f.job.LifecycleGeneration},
			Provider:              "e2b", DaemonFencingToken: "test-fence", BootID: "test-boot",
			TTLExpiresAt: time.Now().UTC().Add(time.Minute), CostReservedUSD: 0.5,
		}, db.ExecBackendCostCap{Configured: true, MaxReservedUSD: 25, PerAttemptUSD: 0.5}); err != nil {
			t.Fatal(err)
		}
		f.run(t)
		f.assertRefusedBeforeProvision(t, remoteReviewAvoidedRetry, 1)
	})
}

func TestRemoteReviewCancellationPreventsLaterProvision(t *testing.T) {
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	key, err := db.ReviewRequestSubjectKey("owner/repo", 2238, f.head, db.DefaultReviewPurpose)
	if err != nil {
		t.Fatal(err)
	}
	if _, won, err := f.store.ClaimReviewRequest(f.ctx, key, f.job.ID, db.DefaultReviewPurpose, "test", reviewRequestOwner()); err != nil || !won {
		t.Fatalf("seed subject claim: won=%v err=%v", won, err)
	}
	if _, err := workflow.CancelJob(f.ctx, f.store, f.job.ID); err != nil {
		t.Fatal(err)
	}

	// Run with the worker's stale queued snapshot: the atomic queued->running
	// admission claim must lose to cancellation and stop before backend setup.
	f.run(t)
	if f.factoryCalls != 0 || f.backend.provisionCalls != 0 {
		t.Fatalf("backend factory/provider calls after cancellation = %d/%d, want 0/0", f.factoryCalls, f.backend.provisionCalls)
	}
	attempts, err := f.store.ListExecBackendAttemptsForJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Fatalf("attempt ledger rows after cancellation = %d, want 0", len(attempts))
	}
	if _, err := f.store.GetReviewRequest(f.ctx, key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("review subject claim after cancellation race: %v, want sql.ErrNoRows", err)
	}
}

func TestRemoteReviewAdmissionAllowsValidExactHeadToProvider(t *testing.T) {
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	key, err := db.ReviewRequestSubjectKey("owner/repo", 2238, f.head, db.DefaultReviewPurpose)
	if err != nil {
		t.Fatal(err)
	}
	if _, won, err := f.store.ClaimReviewRequest(f.ctx, key, f.job.ID, db.DefaultReviewPurpose, "routed-review", reviewRequestOwner()); err != nil || !won {
		t.Fatalf("seed same-job subject claim: won=%v err=%v", won, err)
	}
	f.run(t)
	if f.factoryCalls != 1 || f.backend.provisionCalls != 1 {
		t.Fatalf("backend factory/provider calls = %d/%d, want 1/1", f.factoryCalls, f.backend.provisionCalls)
	}
}

func TestRemoteReviewAdmissionAllowsExplicitOperatorRetry(t *testing.T) {
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	if err := f.store.ReserveExecBackendAttempt(f.ctx, db.ExecBackendAttemptReservation{
		ExecBackendAttemptKey: db.ExecBackendAttemptKey{JobID: f.job.ID, Attempt: 1, LifecycleGeneration: f.job.LifecycleGeneration},
		Provider:              "e2b", DaemonFencingToken: "test-fence", BootID: "test-boot",
		TTLExpiresAt: time.Now().UTC().Add(time.Minute), CostReservedUSD: 0.5,
	}, db.ExecBackendCostCap{Configured: true, MaxReservedUSD: 25, PerAttemptUSD: 0.5}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.TransitionJobStateWithEventAtGeneration(
		f.ctx, f.job.ID, string(workflow.JobQueued), f.job.LifecycleGeneration, string(workflow.JobFailed),
		db.JobEvent{JobID: f.job.ID, Kind: string(workflow.JobFailed), Message: "seed prior failure"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.RetryJob(f.ctx, f.store, f.job.ID); err != nil {
		t.Fatal(err)
	}
	retried, err := f.store.GetJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.job = retried
	f.run(t)
	if f.factoryCalls != 1 || f.backend.provisionCalls != 1 {
		t.Fatalf("backend factory/provider calls = %d/%d, want 1/1 after explicit retry", f.factoryCalls, f.backend.provisionCalls)
	}
}

func TestRemoteReviewProviderRetryClassificationUsesRealClientSignal(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		retryable bool
	}{
		{name: "conflict proves no allocation", status: http.StatusConflict, retryable: true},
		{name: "rate limit remains ambiguous", status: http.StatusTooManyRequests, retryable: false},
		{name: "server error remains ambiguous", status: http.StatusServiceUnavailable, retryable: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, `{"message":"provider response"}`)
			}))
			t.Cleanup(server.Close)
			client, err := e2b.NewClient("test-api-key", e2b.Options{BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, _, providerErr := client.Create(context.Background(), "review-template", time.Minute, e2b.CreateOptions{})
			if providerErr == nil {
				t.Fatal("Create returned nil error")
			}

			f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
			f.worker.recordRetryableRemoteProviderFailure(f.ctx, f.job, providerErr)
			events, err := f.store.ListJobEvents(f.ctx, f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range events {
				found = found || event.Kind == "remote_review_provider_retryable"
			}
			if found != test.retryable {
				t.Fatalf("retryable provider event present = %v, want %v; provider error=%T %v; events=%+v", found, test.retryable, providerErr, providerErr, events)
			}
		})
	}
}

func TestRemoteReviewAdmissionAllowsSuccessfulExplicitCIPolicy(t *testing.T) {
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	file, err := os.OpenFile(f.paths.ConfigFile, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := io.WriteString(file, "\n[review]\nremote_require_ci_green = true\n")
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	f.github.checks = []github.PullRequestCheck{
		{Name: "build", State: "SUCCESS", Bucket: "pass"},
		{Name: "optional", State: "SKIPPED", Bucket: "skipping"},
	}
	f.run(t)
	if f.factoryCalls != 1 || f.backend.provisionCalls != 1 {
		t.Fatalf("backend factory/provider calls = %d/%d, want 1/1", f.factoryCalls, f.backend.provisionCalls)
	}
}

func TestRemoteReviewCancellationAfterAdmissionStopsBeforeProvider(t *testing.T) {
	f := newRemoteReviewAdmissionFixture(t, runtime.ShellRuntime)
	f.beforeBackendFactoryReturn = func() {
		if _, err := workflow.CancelJob(f.ctx, f.store, f.job.ID); err != nil {
			t.Fatalf("CancelJob after admission: %v", err)
		}
		// Let the running-state observer propagate cancellation to the provider
		// context before factory construction returns.
		time.Sleep(2 * daemonJobCancelPollInterval)
	}

	f.run(t)
	if f.factoryCalls != 1 || f.backend.provisionCalls != 0 {
		t.Fatalf("backend factory/provider calls after admitted cancellation = %d/%d, want 1/0", f.factoryCalls, f.backend.provisionCalls)
	}
	attempts, err := f.store.ListExecBackendAttemptsForJob(f.ctx, f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Fatalf("attempt ledger rows after admitted cancellation = %d, want 0", len(attempts))
	}
	if _, err := workflow.RetryJob(f.ctx, f.store, f.job.ID); err != nil {
		t.Fatalf("cancelled admitted review was not settled for retry: %v", err)
	}
}
