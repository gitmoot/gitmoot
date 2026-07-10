package workflow

import (
	"strings"
	"unicode/utf8"

	"github.com/jerryfane/gitmoot/internal/runtime"
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
)

// MaxStderrTailBytes is the hard cap on the stored stderr tail. The tail is
// redacted BEFORE it is cut (so a secret split by the cut can never leak) and
// the stored string never exceeds this many bytes.
const MaxStderrTailBytes = 2048

// FailureDiagnostics captures process-level crash context for a job whose
// runtime session ended WITHOUT producing a gitmoot_result envelope (#806).
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
	// SessionID is the concrete runtime session id in play when one was
	// created/known.
	SessionID string `json:"session_id,omitempty"`
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
	return tailBytes(RedactCommentText(strings.TrimSpace(stderr)), MaxStderrTailBytes)
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
