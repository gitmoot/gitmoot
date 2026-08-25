package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// #1620: when a runtime fails, the job row used to report only
// `phase: streaming, exit_code: 1` plus a stderr tail echoing the skill prompt.
// The runtime's REAL error line ("...requires a newer version of Codex", a
// compaction-endpoint 404) existed solely in `journalctl -u gitmoot-daemon`, so
// two genuinely different faults were indistinguishable on the record and
// operators re-dispatched instead of triaging. These tests bind the daemon's
// journal line to the job row at BOTH terminal-failure call sites.

// A representative provider error that is NOT an operational blocker: it carries
// no 401/429, no quota/rate-limit wording, and no transient-network signature,
// so classifyOperationalBlocker leaves it alone and the run takes the ordinary
// terminal path rather than the #532 pre-terminal deferral.
const deliveryErrorFixture = "400 invalid_request_error: The 'gpt-5.6-sol' model requires a newer version of Codex."

// deliveryErrorSecretFixture puts a token-shaped string in the SAME provider
// error an operator would read. It deliberately avoids "auth"/"invalid"
// co-location so classifyAuthQuotaStrict still declines to classify it.
var deliveryErrorSecretFixture = "Error running remote compact task: unexpected status 404 Not Found, url: https://chatgpt.com/backend-api/codex/responses/compact?secret=" + deliveryErrorSecret

var deliveryErrorSecret = "ghp_" + strings.Repeat("A", 36)

// lockedBuffer is a race-safe stdout sink: the worker's writeLine calls are the
// daemon's journal lines, and this test reads them back to prove the row carries
// the same text.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// journalFailureText extracts what the daemon logged as "job <id> failed: <err>"
// — the string this feature exists to stop losing.
func journalFailureText(t *testing.T, stdout string, jobID string) string {
	t.Helper()
	prefix := fmt.Sprintf("job %s failed: ", jobID)
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	t.Fatalf("daemon stdout has no %q line:\n%s", prefix, stdout)
	return ""
}

func deliveryErrorDiagnostics(t *testing.T, store *db.Store, jobID string) *workflow.FailureDiagnostics {
	t.Helper()
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob(%s) returned error: %v", jobID, err)
	}
	payload, err := daemonJobPayload(job)
	if err != nil {
		t.Fatalf("daemonJobPayload(%s) returned error: %v", jobID, err)
	}
	return payload.FailureDiagnostics
}

// runFailingDelivery drives the ORDINARY worker path (daemon_worker.go's run)
// to a terminal failure with a caller-chosen delivery error, returning the
// daemon's own journal line for it.
func runFailingDelivery(t *testing.T, store *db.Store, home string, jobID string, deliveryErr error) string {
	t.Helper()
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: jobID, Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main",
	})
	job := mustWorkerJob(t, store, jobID)

	stdout := &lockedBuffer{}
	worker := defaultJobWorker(store, stdout, home)
	worker.CheckoutValidator = func(context.Context, db.Job, workflow.JobPayload, runtime.Agent) (string, error) {
		return t.TempDir(), nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{err: deliveryErr}, nil
	}
	if err := worker.run(context.Background(), job); err != nil {
		t.Fatalf("worker.run returned error: %v", err)
	}
	after := mustWorkerJob(t, store, jobID)
	if after.State != string(workflow.JobFailed) {
		t.Fatalf("state = %q, want failed (a deferral or block would make this test vacuous)", after.State)
	}
	return journalFailureText(t, stdout.String(), jobID)
}

func TestRunRecordsDeliveryErrorOnTheJobRow(t *testing.T) {
	store := daemonWorkerStore(t)
	journal := runFailingDelivery(t, store, t.TempDir(), "job-delivery-error", errors.New(deliveryErrorFixture))

	diag := deliveryErrorDiagnostics(t, store, "job-delivery-error")
	if diag == nil {
		t.Fatal("FailureDiagnostics = nil, want the delivery error recorded on the row")
	}
	if !strings.Contains(diag.DeliveryError, deliveryErrorFixture) {
		t.Fatalf("delivery_error = %q, want it to carry the runtime's own error", diag.DeliveryError)
	}
	// The whole point: the row now says what the journal said. Substring, not
	// equality, so a future wrapper adding context to the error keeps the binding
	// meaningful instead of turning it into a churn magnet.
	if !strings.Contains(diag.DeliveryError, journal) {
		t.Fatalf("delivery_error = %q, want it to carry the daemon's journal text %q", diag.DeliveryError, journal)
	}

	var buf bytes.Buffer
	printFailureDiagnostics(&buf, diag)
	out := buf.String()
	if !strings.Contains(out, "  delivery_error:") || !strings.Contains(out, deliveryErrorFixture) {
		t.Fatalf("job show output missing the delivery error:\n%s", out)
	}
}

// The guard with a real consequence: an unredacted provider error on a durable
// job row is a token-leak surface. Removing the redaction call anywhere on this
// field's write path turns this test RED.
func TestRunRedactsDeliveryErrorOnTheJobRow(t *testing.T) {
	store := daemonWorkerStore(t)
	journal := runFailingDelivery(t, store, t.TempDir(), "job-delivery-secret", errors.New(deliveryErrorSecretFixture))

	diag := deliveryErrorDiagnostics(t, store, "job-delivery-secret")
	if diag == nil {
		t.Fatal("FailureDiagnostics = nil, want the delivery error recorded on the row")
	}
	if !strings.Contains(journal, deliveryErrorSecret) {
		t.Fatalf("journal line = %q, want the RAW token (otherwise this test proves nothing)", journal)
	}
	if strings.Contains(diag.DeliveryError, deliveryErrorSecret) {
		t.Fatalf("delivery_error leaked the token: %q", diag.DeliveryError)
	}
	if !strings.Contains(diag.DeliveryError, "[REDACTED]") {
		t.Fatalf("delivery_error = %q, want the redaction marker", diag.DeliveryError)
	}
	if !strings.Contains(diag.DeliveryError, "unexpected status 404 Not Found") {
		t.Fatalf("delivery_error = %q, want the triage-bearing text kept", diag.DeliveryError)
	}
	// Byte-for-byte what StderrTail's own path produces for the same input: the
	// new field must not acquire a second, weaker redaction path. If the write
	// path ever skips redaction the stored text has no "[REDACTED]" for this
	// already-redacted string to match against, so this fails too.
	if want := workflow.WithDeliveryError(nil, journal).DeliveryError; !strings.Contains(diag.DeliveryError, want) {
		t.Fatalf("delivery_error = %q, want it to carry the stderr-tail redaction result %q", diag.DeliveryError, want)
	}
}

// The delegated (temp-worker) terminal path is a SECOND call site and needs its
// own case: a delegated run's runtime error was journal-only too.
func TestTempWorkerRunRecordsDeliveryErrorOnTheJobRow(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	checkout := t.TempDir()
	runGit(t, checkout, "init")
	runGit(t, checkout, "branch", "-m", "main")
	runGit(t, checkout, "remote", "add", "origin", "https://github.com/owner/repo.git")
	seedDaemonWorkerRepo(t, store, "owner/repo", checkout)
	original := runtime.Agent{
		Name: "codex-reviewer", Role: "worker", Runtime: runtime.CodexRuntime,
		RuntimeRef: "550e8400-e29b-41d4-a716-446655440002", RepoScope: "owner/repo",
		Capabilities: []string{"ask"}, AutonomyPolicy: runtime.AutonomyPolicyAuto,
	}
	seedDaemonWorkerAgent(t, store, original.Name, original.Runtime, original.RuntimeRef, original.Capabilities, original.RepoScope)
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-temp-delivery-error", Agent: original.Name, Action: "ask", Repo: "owner/repo",
	})
	admitted := mustWorkerJob(t, store, "job-temp-delivery-error")
	payload, err := daemonJobPayload(admitted)
	if err != nil {
		t.Fatalf("daemonJobPayload returned error: %v", err)
	}

	stdout := &lockedBuffer{}
	worker := defaultJobWorker(store, stdout, t.TempDir())
	worker.StartAdapterFactory = func(execbackend.Backend, string, string) (runtime.Adapter, error) {
		return &cliWorkerFakeAdapter{startRuntimeRef: "550e8400-e29b-41d4-a716-446655440003"}, nil
	}
	worker.AdapterFactory = func(runtime.Agent, string) (workflow.DeliveryAdapter, error) {
		return &cliWorkerFakeAdapter{err: errors.New(deliveryErrorFixture)}, nil
	}
	policy := config.DefaultParallelSessionPolicy()
	policy.MergeBack = config.ParallelSessionMergeBackOff
	if err := worker.runWithTempWorker(ctx, admitted, payload, execbackend.Local, original, checkout, policy, "test contention", false); err != nil {
		t.Fatalf("runWithTempWorker returned error: %v", err)
	}

	after := mustWorkerJob(t, store, admitted.ID)
	if after.State != string(workflow.JobFailed) {
		t.Fatalf("state = %q, want failed", after.State)
	}
	diag := deliveryErrorDiagnostics(t, store, admitted.ID)
	if diag == nil {
		t.Fatal("FailureDiagnostics = nil, want the delegated run's delivery error on the row")
	}
	if !strings.Contains(diag.DeliveryError, deliveryErrorFixture) {
		t.Fatalf("delivery_error = %q, want the delegated runtime's own error", diag.DeliveryError)
	}
	if journal := journalFailureText(t, stdout.String(), admitted.ID); !strings.Contains(diag.DeliveryError, journal) {
		t.Fatalf("delivery_error = %q, want it to carry the daemon's journal text %q", diag.DeliveryError, journal)
	}
}

// A job that fails with an empty error must not grow an empty/garbage field, and
// must not conjure a diagnostics block out of nothing.
func TestRecordDeliveryFailureDiagnosticsIgnoresEmptyError(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-empty-delivery-error", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main",
	})
	job := mustWorkerJob(t, store, "job-empty-delivery-error")
	worker := defaultJobWorker(store, io.Discard, t.TempDir())

	worker.recordDeliveryFailureDiagnostics(ctx, job.ID, job.LifecycleGeneration, errors.New(""))
	worker.recordDeliveryFailureDiagnostics(ctx, job.ID, job.LifecycleGeneration, errors.New("   \n\t "))
	worker.recordDeliveryFailureDiagnostics(ctx, job.ID, job.LifecycleGeneration, nil)

	if diag := deliveryErrorDiagnostics(t, store, job.ID); diag != nil {
		t.Fatalf("FailureDiagnostics = %+v, want none for an empty delivery error", diag)
	}
	stored := mustWorkerJob(t, store, job.ID)
	if strings.Contains(stored.Payload, "delivery_error") {
		t.Fatalf("payload = %q, want no delivery_error key", stored.Payload)
	}
}

// A retry that claimed the row between the run error and this write owns a NEWER
// generation and has already cleared diagnostics for its own run; stamping the
// dead run's error onto it would be a live-row corruption, so the write drops.
func TestRecordDeliveryFailureDiagnosticsSkipsSupersededGeneration(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-superseded-delivery-error", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main",
	})
	job := mustWorkerJob(t, store, "job-superseded-delivery-error")
	worker := defaultJobWorker(store, io.Discard, t.TempDir())

	worker.recordDeliveryFailureDiagnostics(ctx, job.ID, job.LifecycleGeneration-1, errors.New(deliveryErrorFixture))

	if diag := deliveryErrorDiagnostics(t, store, job.ID); diag != nil {
		t.Fatalf("FailureDiagnostics = %+v, want none: a superseded run must not write", diag)
	}
}
