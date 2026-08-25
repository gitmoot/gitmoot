package workflow

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// shellCrashAgent builds a shell-runtime agent whose session command is the
// given script, delivered by the REAL shell adapter (real process, real exit
// code) so the crash-diagnostics capture is proven at the runtime boundary.
func shellCrashAgent(script string) runtime.Agent {
	return runtime.Agent{Name: "audit", Runtime: runtime.ShellRuntime, RuntimeRef: script, RepoScope: "gitmoot/gitmoot", Role: "reviewer"}
}

func TestMailboxRunCapturesCrashDiagnosticsOnNonZeroExit(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := shellCrashAgent(`echo "api_key=super-secret-crash-value" >&2; echo "runtime exploded" >&2; exit 7`)

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-crash", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-crash", agent, runtime.ShellAdapter{}); err == nil {
		t.Fatal("Run succeeded despite the runtime exiting non-zero")
	}

	stored, err := store.GetJob(ctx, "job-crash")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobFailed) {
		t.Fatalf("state = %q, want failed", stored.State)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	diag := payload.FailureDiagnostics
	if diag == nil {
		t.Fatal("payload.FailureDiagnostics = nil, want crash diagnostics")
	}
	if diag.Phase != FailurePhaseLaunched {
		t.Fatalf("phase = %q, want %q (the CLI produced no stdout)", diag.Phase, FailurePhaseLaunched)
	}
	if diag.ExitCode == nil || *diag.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7", diag.ExitCode)
	}
	if diag.Signal != "" {
		t.Fatalf("signal = %q, want empty for a plain exit", diag.Signal)
	}
	if !strings.Contains(diag.StderrTail, "runtime exploded") {
		t.Fatalf("stderr tail = %q, want the captured stderr", diag.StderrTail)
	}
	if strings.Contains(diag.StderrTail, "super-secret-crash-value") {
		t.Fatalf("stderr tail leaked the secret: %q", diag.StderrTail)
	}
	if !strings.Contains(diag.StderrTail, "[REDACTED]") {
		t.Fatalf("stderr tail = %q, want the redaction marker", diag.StderrTail)
	}
	events, err := store.ListJobEvents(ctx, "job-crash")
	if err != nil {
		t.Fatalf("ListJobEvents: %v", err)
	}
	failedMessage := events[len(events)-1].Message
	if !strings.Contains(failedMessage, "stderr tail:") || !strings.Contains(failedMessage, "runtime exploded") || strings.Contains(failedMessage, "super-secret-crash-value") {
		t.Fatalf("failed event diagnostics = %q", failedMessage)
	}
}

func TestMailboxRunCrashDiagnosticsStreamingPhase(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := shellCrashAgent(`echo "partial stdout"; echo "died mid-stream" >&2; exit 3`)

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-stream", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-stream", agent, runtime.ShellAdapter{}); err == nil {
		t.Fatal("Run succeeded despite the runtime exiting non-zero")
	}

	stored, err := store.GetJob(ctx, "job-stream")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	diag := payload.FailureDiagnostics
	if diag == nil {
		t.Fatal("payload.FailureDiagnostics = nil, want crash diagnostics")
	}
	if diag.Phase != FailurePhaseStreaming {
		t.Fatalf("phase = %q, want %q (the CLI produced stdout before dying)", diag.Phase, FailurePhaseStreaming)
	}
	if diag.ExitCode == nil || *diag.ExitCode != 3 {
		t.Fatalf("exit code = %v, want 3", diag.ExitCode)
	}
	if !strings.Contains(diag.StderrTail, "died mid-stream") {
		t.Fatalf("stderr tail = %q", diag.StderrTail)
	}
}

func TestMailboxRunRecordsResultParseDiagnostics(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := shellCrashAgent(`echo "no envelope from me"; echo "warned on stderr" >&2`)

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-parse", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-parse", agent, runtime.ShellAdapter{}); err == nil {
		t.Fatal("Run succeeded despite the output never carrying an envelope")
	}

	stored, err := store.GetJob(ctx, "job-parse")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobFailed) {
		t.Fatalf("state = %q, want failed", stored.State)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	diag := payload.FailureDiagnostics
	if diag == nil {
		t.Fatal("payload.FailureDiagnostics = nil, want result-parse diagnostics")
	}
	if diag.Phase != FailurePhaseResultParse {
		t.Fatalf("phase = %q, want %q", diag.Phase, FailurePhaseResultParse)
	}
	if diag.ExitCode == nil || *diag.ExitCode != 0 {
		t.Fatalf("exit code = %v, want 0 (the deliveries completed)", diag.ExitCode)
	}
	if !strings.Contains(diag.StderrTail, "warned on stderr") {
		t.Fatalf("stderr tail = %q", diag.StderrTail)
	}
}

func TestMailboxRunSuccessStoresNoFailureDiagnostics(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := shellCrashAgent(`echo '{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`)

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-ok", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	if _, err := mailbox.Run(ctx, "job-ok", agent, runtime.ShellAdapter{}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	stored, err := store.GetJob(ctx, "job-ok")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	if stored.State != string(JobSucceeded) {
		t.Fatalf("state = %q, want succeeded", stored.State)
	}
	if strings.Contains(stored.Payload, "failure_diagnostics") {
		t.Fatalf("payload = %q, want no failure_diagnostics on success", stored.Payload)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.FailureDiagnostics != nil {
		t.Fatalf("FailureDiagnostics = %+v, want nil", payload.FailureDiagnostics)
	}
}

func TestMailboxRunClearsStaleFailureDiagnosticsOnRetry(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	mailbox := Mailbox{store: store, resolveDeliveryWorktree: ExcludedDeliveryWorktreeResolver("test_explicit_no_worktree")}
	agent := shellCrashAgent(`echo '{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}'`)

	if _, err := mailbox.Enqueue(ctx, JobRequest{ID: "job-retry", Agent: "audit", Action: "review", Repo: "gitmoot/gitmoot"}); err != nil {
		t.Fatalf("Enqueue returned error: %v", err)
	}
	// Seed a stale crash report from a previous run onto the queued payload.
	stored, err := store.GetJob(ctx, "job-retry")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err := unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	staleCode := 9
	payload.FailureDiagnostics = &FailureDiagnostics{Phase: FailurePhaseLaunched, ExitCode: &staleCode, StderrTail: "stale crash"}
	encoded, err := marshalPayload(payload)
	if err != nil {
		t.Fatalf("marshalPayload returned error: %v", err)
	}
	if err := store.UpdateJobPayload(ctx, "job-retry", encoded); err != nil {
		t.Fatalf("UpdateJobPayload returned error: %v", err)
	}

	if _, err := mailbox.Run(ctx, "job-retry", agent, runtime.ShellAdapter{}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	stored, err = store.GetJob(ctx, "job-retry")
	if err != nil {
		t.Fatalf("GetJob returned error: %v", err)
	}
	payload, err = unmarshalPayload(stored.Payload)
	if err != nil {
		t.Fatalf("unmarshalPayload returned error: %v", err)
	}
	if payload.FailureDiagnostics != nil {
		t.Fatalf("FailureDiagnostics = %+v, want the stale crash report cleared by the successful retry", payload.FailureDiagnostics)
	}
}

func TestRedactedStderrTailBoundsAndRedacts(t *testing.T) {
	padding := strings.Repeat("x", 4*MaxStderrTailBytes)
	secret := "token=ghp_" + strings.Repeat("a", 30)
	in := padding + "\n" + secret + "\ntail-marker-end"

	out := redactedStderrTail(in)

	if len(out) > MaxStderrTailBytes {
		t.Fatalf("len = %d, want <= %d", len(out), MaxStderrTailBytes)
	}
	if !strings.HasSuffix(out, "tail-marker-end") {
		t.Fatalf("out = %q, want the tail end retained", out)
	}
	if strings.Contains(out, "ghp_") {
		t.Fatalf("out leaked the token: %q", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("out = %q, want the redaction marker", out)
	}
}

// The engine's own delivery error is a DIFFERENT source from the CLI's stderr,
// but it is exactly as likely to carry a token, a URL, or a request id — so it
// must go through the identical redaction+bound path (#1620). This is the guard
// with a real consequence: an unredacted provider error on a durable job row is
// a token-leak surface.
func TestWithDeliveryErrorRedactsThroughTheStderrTailPath(t *testing.T) {
	secret := "ghp_" + strings.Repeat("A", 36)
	detail := "400 invalid_request_error: rejected (authorization: Bearer " + secret + ")"

	diag := WithDeliveryError(nil, detail)

	if diag == nil {
		t.Fatal("WithDeliveryError(nil, <detail>) = nil, want an allocated diagnostics")
	}
	if strings.Contains(diag.DeliveryError, secret) {
		t.Fatalf("delivery error leaked the token: %q", diag.DeliveryError)
	}
	if !strings.Contains(diag.DeliveryError, "[REDACTED]") {
		t.Fatalf("delivery error = %q, want the redaction marker", diag.DeliveryError)
	}
	if !strings.Contains(diag.DeliveryError, "400 invalid_request_error") {
		t.Fatalf("delivery error = %q, want the triage-bearing text kept", diag.DeliveryError)
	}
	// Identical to what StderrTail would do with the same input — the field must
	// not acquire its own, weaker redaction path.
	if want := redactedStderrTail(detail); diag.DeliveryError != want {
		t.Fatalf("delivery error = %q, want the stderr-tail redaction result %q", diag.DeliveryError, want)
	}
}

func TestWithDeliveryErrorBoundsLikeStderrTail(t *testing.T) {
	detail := strings.Repeat("x", 4*MaxStderrTailBytes) + "\ntail-marker-end"

	diag := WithDeliveryError(nil, detail)

	if len(diag.DeliveryError) > MaxStderrTailBytes {
		t.Fatalf("len = %d, want <= %d", len(diag.DeliveryError), MaxStderrTailBytes)
	}
	if !strings.HasSuffix(diag.DeliveryError, "tail-marker-end") {
		t.Fatalf("delivery error (%d bytes) does not end at the tail marker", len(diag.DeliveryError))
	}
}

// A delivery that failed with an empty error must not grow an empty/garbage
// field, and must not conjure a diagnostics block out of nothing.
func TestWithDeliveryErrorIgnoresBlankDetail(t *testing.T) {
	if diag := WithDeliveryError(nil, ""); diag != nil {
		t.Fatalf("WithDeliveryError(nil, \"\") = %+v, want nil", diag)
	}
	if diag := WithDeliveryError(nil, "   \n\t "); diag != nil {
		t.Fatalf("WithDeliveryError(nil, whitespace) = %+v, want nil", diag)
	}
	existing := &FailureDiagnostics{Phase: FailurePhaseStreaming, StderrTail: "prompt echo"}
	if diag := WithDeliveryError(existing, " "); diag.DeliveryError != "" {
		t.Fatalf("delivery error = %q, want it left unset by a blank detail", diag.DeliveryError)
	}
}

// Additive only: the crash evidence a job already carried survives untouched.
func TestWithDeliveryErrorPreservesExistingDiagnostics(t *testing.T) {
	exitCode := 1
	existing := &FailureDiagnostics{
		Phase: FailurePhaseStreaming, ExitCode: &exitCode, Signal: "SIGKILL",
		StderrTail: "echo of the skill system prompt", SessionID: "sess-9",
	}

	diag := WithDeliveryError(existing, "unexpected status 404 Not Found")

	if diag != existing {
		t.Fatalf("WithDeliveryError returned a different diagnostics pointer")
	}
	if diag.Phase != FailurePhaseStreaming || diag.ExitCode == nil || *diag.ExitCode != 1 ||
		diag.Signal != "SIGKILL" || diag.StderrTail != "echo of the skill system prompt" || diag.SessionID != "sess-9" {
		t.Fatalf("existing diagnostics disturbed: %+v", diag)
	}
	if diag.DeliveryError != "unexpected status 404 Not Found" {
		t.Fatalf("delivery error = %q", diag.DeliveryError)
	}
}

func TestTailBytesKeepsValidUTF8(t *testing.T) {
	// 3-byte runes force the byte cut to land mid-rune, exercising the
	// boundary re-alignment.
	in := strings.Repeat("€", MaxStderrTailBytes)

	out := tailBytes(in, MaxStderrTailBytes)

	if len(out) > MaxStderrTailBytes {
		t.Fatalf("len = %d, want <= %d", len(out), MaxStderrTailBytes)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("out is not valid UTF-8: %q", out[:12])
	}
}
