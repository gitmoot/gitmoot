package workflow

import (
	"strings"
	"unicode/utf8"

	"github.com/gitmoot/gitmoot/internal/runtime"
)

// Failure-diagnostics phase markers (#806): how far the runtime session got
// before it ended without producing a gitmoot_result envelope.
const (
	// FailurePhaseLaunched: the CLI process ran but ended before producing any
	// stdout (it may still have written stderr).
	FailurePhaseLaunched = "launched"
	// FailurePhaseStreaming: the CLI produced output but the delivery still
	// failed (the process died or exited non-zero mid-stream).
	FailurePhaseStreaming = "streaming"
	// FailurePhaseResultParse: every delivery completed but no valid
	// gitmoot_result envelope was found in the output, repairs included.
	FailurePhaseResultParse = "result-parse"
	// FailurePhaseRecovery: the daemon found a durable running row whose worker
	// was proven gone during boot or stale-run recovery.
	FailurePhaseRecovery = "recovery"
)

// MaxStderrTailBytes is the hard cap on the stored stderr tail. The tail is
// redacted BEFORE it is cut (so a secret split by the cut can never leak) and
// the stored string never exceeds this many bytes.
const MaxStderrTailBytes = 4096

// FailurePhaseFinalized: the job was terminalized by the ENGINE after the fact,
// without a stored result and without session evidence - a spent deadline, a
// supersession, a refusal before it ran, or a mid-run runtime failure whose
// delivery never recorded anything. It is a distinct phase from the four above
// because no CLI process state was observed: the marker says "the engine ended
// this", which is exactly what 169 delegation legs in one store could not say.
const FailurePhaseFinalized = "finalized"

// FailureDiagnostics captures process-level crash context for a job whose
// runtime session ended WITHOUT producing a gitmoot_result envelope (#806), or
// whose durable running row was terminally settled by daemon recovery (#1308).
// It lives inside the job payload JSON (additive, omitempty) — no schema
// change — and is cleared at the start of every run so a retried job never
// carries a previous run's crash report. Successful jobs never store one.
type FailureDiagnostics struct {
	// Phase is one of the FailurePhase* markers above.
	Phase string `json:"phase"`
	// ExitCode is the runtime CLI's process exit status when known; omitted
	// when the process was signal-terminated or never reported one.
	ExitCode *int `json:"exit_code,omitempty"`
	// Signal is the signal name when the process was terminated by a signal.
	Signal string `json:"signal,omitempty"`
	// StderrTail is the redacted last <= MaxStderrTailBytes bytes of the CLI's
	// stderr, run through the same token-redaction rules as GitHub job comments
	// and bug reports before storage.
	StderrTail string `json:"stderr_tail,omitempty"`
	// DeliveryError is the redacted text of the ENGINE's own terminal error for
	// the delivery — the very string the daemon logs as "job <id> failed: <err>"
	// (#1620). It is deliberately NOT folded into StderrTail: that field means
	// "tail of the runtime CLI's stderr", which is a different source and often
	// carries only an echo of the prompt. Before this field the daemon journal
	// held the only copy, so a model/CLI mismatch and a compaction-endpoint 404
	// both read as an identical `phase: streaming, exit_code: 1` on the job row
	// and operators re-dispatched instead of triaging. Set only through
	// WithDeliveryError, which is what keeps redaction from being bypassed.
	DeliveryError string `json:"delivery_error,omitempty"`
	// SessionID is the concrete runtime session id in play when one was
	// created/known.
	SessionID string `json:"session_id,omitempty"`
}

// WithDeliveryError records the engine's terminal delivery error on diag and
// returns it. The detail runs through redactedStderrTail — the IDENTICAL
// redaction-then-bound path StderrTail uses — because a provider error routinely
// carries a URL, a request id, or a token, and a job row is a durable, widely
// read surface.
//
// A blank detail (or one that redacts away to nothing) is a no-op that never
// allocates: a job whose delivery failed with an empty error must not grow an
// empty diagnostics block. A nil diag with a real detail allocates a PHASE-LESS
// FailureDiagnostics rather than inventing a phase — the delivery failed without
// the adapter reporting session evidence, so there is no phase to claim.
func WithDeliveryError(diag *FailureDiagnostics, detail string) *FailureDiagnostics {
	redacted := redactedStderrTail(detail)
	if redacted == "" {
		return diag
	}
	if diag == nil {
		diag = &FailureDiagnostics{}
	}
	diag.DeliveryError = redacted
	return diag
}

// failureDiagnosticsFromSession converts an adapter's raw session evidence into
// the storable, redacted, bounded form. The phase defaults to launched/streaming
// from whether the CLI produced stdout; the result-parse terminal overrides it.
// nil in (no CLI process ran) is nil out.
func failureDiagnosticsFromSession(diag *runtime.SessionDiag) *FailureDiagnostics {
	if diag == nil {
		return nil
	}
	phase := FailurePhaseLaunched
	if diag.StdoutSeen {
		phase = FailurePhaseStreaming
	}
	out := &FailureDiagnostics{
		Phase:      phase,
		Signal:     diag.Signal,
		StderrTail: redactedStderrTail(diag.Stderr),
		SessionID:  strings.TrimSpace(diag.SessionID),
	}
	if diag.ExitCode != nil {
		code := *diag.ExitCode
		out.ExitCode = &code
	}
	return out
}

// redactedStderrTail bounds and redacts a runtime CLI's stderr for storage.
// Redaction runs over the FULL text first — tailing first could split a token
// so the redactor no longer matches it, leaking a partial secret — then only
// the last MaxStderrTailBytes bytes are kept, aligned to a rune boundary.
func redactedStderrTail(stderr string) string {
	return RedactedStderrTail(stderr)
}

// RedactedStderrTail applies the workflow's shared bounded-error redaction.
// Explicit secrets cover provider-specific credentials whose format is not
// recognizable by the generic token patterns.
func RedactedStderrTail(stderr string, secrets ...string) string {
	stderr = strings.TrimSpace(stderr)
	for _, secret := range secrets {
		if secret != "" {
			stderr = strings.ReplaceAll(stderr, secret, "[REDACTED]")
		}
	}
	return tailBytes(RedactCommentText(stderr), MaxStderrTailBytes)
}

// tailBytes returns the trailing at-most-max bytes of s, advanced to the next
// rune boundary so the result is valid UTF-8.
func tailBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[len(s)-max:]
	for i := 0; i < len(cut); i++ {
		if utf8.RuneStart(cut[i]) {
			return cut[i:]
		}
	}
	return ""
}
