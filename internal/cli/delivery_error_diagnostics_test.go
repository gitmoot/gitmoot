package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
//
// It drops SILENTLY, and that is asserted here rather than left implicit: a
// superseded write is the expected outcome of an ordinary retry, and announcing it
// on the journal every time would be the noise that teaches operators to skip the
// line the persistence-failure case needs them to read.
func TestRecordDeliveryFailureDiagnosticsSkipsSupersededGeneration(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-superseded-delivery-error", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main",
	})
	job := mustWorkerJob(t, store, "job-superseded-delivery-error")
	stdout := &lockedBuffer{}
	worker := defaultJobWorker(store, stdout, t.TempDir())

	worker.recordDeliveryFailureDiagnostics(ctx, job.ID, job.LifecycleGeneration-1, errors.New(deliveryErrorFixture))

	if diag := deliveryErrorDiagnostics(t, store, job.ID); diag != nil {
		t.Fatalf("FailureDiagnostics = %+v, want none: a superseded run must not write", diag)
	}
	if out := stdout.String(); strings.Contains(out, deliveryDiagnosticsLossPrefix) {
		t.Fatalf("stdout = %q, want silence: a superseded generation is an EXPECTED drop", out)
	}
}

// deliveryDiagnosticsLossPrefix is the journal marker for "the delivery error
// never reached the row". Both the silence assertion above and the fault
// assertions below key off it.
const deliveryDiagnosticsLossPrefix = "delivery-error diagnostics not recorded"

// liveGenerationTail marks the payload a RETRY installed for its own run. The
// stale observer must leave it exactly as found.
const liveGenerationTail = "live generation stderr"

// TestRecordDeliveryFailureDiagnosticsLosesTheRaceToANewGeneration is the probe
// for the defect this round closes: the generation check and the payload write
// were two statements, so a retry landing BETWEEN them left a stale observer
// overwriting the new generation's row.
//
// The superseded-generation test above cannot catch that. It advances the
// generation BEFORE the call, so the early check rejects it and the write is never
// reached — an unconditional write passes that test. Only a retry that lands
// INSIDE the check-to-write window discriminates, which is what the
// beforeDeliveryDiagnosticsWrite seam exists to stage. Reverting the write to the
// unconditional UpdateJobPayload turns this test RED and nothing else.
func TestRecordDeliveryFailureDiagnosticsLosesTheRaceToANewGeneration(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-raced-delivery-error", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main",
	})
	// queued -> running -> failed leaves the generation alone (only entry to queued
	// is a new run), so the observer below genuinely holds the run that failed.
	for _, step := range [][2]string{{"queued", "running"}, {"running", "failed"}} {
		transitioned, err := store.TransitionJobState(ctx, "job-raced-delivery-error", step[0], step[1])
		if err != nil {
			t.Fatalf("TransitionJobState(%s->%s) returned error: %v", step[0], step[1], err)
		}
		if !transitioned {
			t.Fatalf("TransitionJobState(%s->%s) did not transition; the fixture is not in the shape this probe needs", step[0], step[1])
		}
	}
	observed := mustWorkerJob(t, store, "job-raced-delivery-error")

	// What the retry installs for ITS run: its own diagnostics, no delivery_error.
	livePayload, err := workflow.ParseJobPayload(observed.Payload)
	if err != nil {
		t.Fatalf("ParseJobPayload returned error: %v", err)
	}
	livePayload.FailureDiagnostics = &workflow.FailureDiagnostics{
		Phase: workflow.FailurePhaseLaunched, StderrTail: liveGenerationTail,
	}
	liveEncoded, err := json.Marshal(livePayload)
	if err != nil {
		t.Fatalf("Marshal(live payload) returned error: %v", err)
	}

	stdout := &lockedBuffer{}
	worker := defaultJobWorker(store, stdout, t.TempDir())
	fired := false
	worker.beforeDeliveryDiagnosticsWrite = func() {
		fired = true
		// failed -> queued is the operator retry: it bumps the generation and
		// installs the new run's payload, all after the observer's check passed.
		if err := store.UpdateJobPayloadAndStateWithEvent(ctx, observed.ID, string(liveEncoded), "queued",
			db.JobEvent{JobID: observed.ID, Kind: "queued", Message: "retry"}); err != nil {
			t.Errorf("UpdateJobPayloadAndStateWithEvent(retry) returned error: %v", err)
		}
	}

	worker.recordDeliveryFailureDiagnostics(ctx, observed.ID, observed.LifecycleGeneration, errors.New(deliveryErrorFixture))

	if !fired {
		t.Fatal("the race window never opened; the probe proves nothing")
	}
	after := mustWorkerJob(t, store, observed.ID)
	if after.LifecycleGeneration != observed.LifecycleGeneration+1 {
		t.Fatalf("generation = %d, want %d (the retry did not arm)", after.LifecycleGeneration, observed.LifecycleGeneration+1)
	}
	// Byte equality, because "unmodified" is the claim: the stale write must not
	// have touched ANY part of the live generation's payload.
	if after.Payload != string(liveEncoded) {
		t.Fatalf("payload = %q, want the live generation's own payload %q -- a stale observer overwrote a live run",
			after.Payload, string(liveEncoded))
	}
	diag := deliveryErrorDiagnostics(t, store, observed.ID)
	if diag == nil || diag.StderrTail != liveGenerationTail {
		t.Fatalf("FailureDiagnostics = %+v, want the live run's own diagnostics kept", diag)
	}
	if diag.DeliveryError != "" {
		t.Fatalf("delivery_error = %q, want empty: a dead run's error must not land on a live run's row", diag.DeliveryError)
	}
	// The lost CAS is the superseded case arriving at the write, so it is as quiet
	// as the check is.
	if out := stdout.String(); strings.Contains(out, deliveryDiagnosticsLossPrefix) {
		t.Fatalf("stdout = %q, want silence: losing the CAS is an EXPECTED drop", out)
	}
}

// The other half of the split: a persistence FAULT is not expected, so dropping it
// silently would leave the row with no delivery_error and no explanation anywhere
// — the journal-versus-row blindness of #1620, one level up. The store is closed
// inside the write window so the CAS itself fails, which is the arm that matters:
// the read, the parse and the marshal have all already succeeded.
func TestRecordDeliveryFailureDiagnosticsReportsPersistenceFailure(t *testing.T) {
	ctx := context.Background()
	store := daemonWorkerStore(t)
	seedDaemonWorkerAgent(t, store, "audit", runtime.ShellRuntime, "", []string{"ask"}, "owner/repo")
	enqueueDaemonWorkerJob(t, store, workflow.JobRequest{
		ID: "job-unpersisted-delivery-error", Agent: "audit", Action: "ask", Repo: "owner/repo", Branch: "main",
	})
	job := mustWorkerJob(t, store, "job-unpersisted-delivery-error")

	stdout := &lockedBuffer{}
	worker := defaultJobWorker(store, stdout, t.TempDir())
	worker.beforeDeliveryDiagnosticsWrite = func() {
		// A closed store is a persistence fault the write cannot survive, injected
		// after every earlier step has already succeeded.
		if err := store.Close(); err != nil {
			t.Errorf("Close returned error: %v", err)
		}
	}

	worker.recordDeliveryFailureDiagnostics(ctx, job.ID, job.LifecycleGeneration, errors.New(deliveryErrorSecretFixture))

	out := stdout.String()
	if !strings.Contains(out, deliveryDiagnosticsLossPrefix) {
		t.Fatalf("stdout = %q, want the %q diagnostic for a persistence fault", out, deliveryDiagnosticsLossPrefix)
	}
	if !strings.Contains(out, job.ID) {
		t.Fatalf("stdout = %q, want the job id so an operator can find the row", out)
	}
	// The journal line quotes the delivery error, so it is a leak surface exactly
	// like the stored field. Same redaction, not a second weaker path.
	if strings.Contains(out, deliveryErrorSecret) {
		t.Fatalf("the diagnostic leaked the token: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("stdout = %q, want the redaction marker", out)
	}
	if !strings.Contains(out, "unexpected status 404 Not Found") {
		t.Fatalf("stdout = %q, want the triage-bearing text kept", out)
	}
}
