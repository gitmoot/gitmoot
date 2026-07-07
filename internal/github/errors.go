package github

import (
	"errors"
	"strings"

	"github.com/jerryfane/gitmoot/internal/subprocess"
)

// TransientError marks a gh-CLI failure that a network/GitHub outage caused —
// connection refused/reset, DNS/TLS failure, a gateway 5xx, a timeout — as
// opposed to a deterministic failure (bad request, 404, permission, a genuine
// conflict) that retrying would not fix. It is the minimal typed discriminator
// #532 slice D calls for: the daemon's operational-blocker classifier maps it (or
// its signature via IsTransientMessage) to the network_outage class with a short
// backoff, but ONLY where a github operation currently TERMINALLY fails a job —
// best-effort github paths swallow the error and stay best-effort.
//
// Error() is transparent (it forwards to the wrapped error) so every existing
// string-based consumer of a gh failure — logs, comments, the #552 stuck-reason
// matcher — stays byte-identical; the marker is observed only via errors.As /
// AsTransient. It mirrors the existing UpdatePullRequestBranchError transient
// discriminator, generalized to any gh call routed through commandError.
type TransientError struct{ Err error }

func (e TransientError) Error() string {
	if e.Err == nil {
		return "transient github error"
	}
	return e.Err.Error()
}

func (e TransientError) Unwrap() error { return e.Err }

// AsTransient reports whether err (anywhere in its chain) is a TransientError.
func AsTransient(err error) bool {
	var t TransientError
	return errors.As(err, &t)
}

// IsTransientMessage reports whether a gh-CLI failure message carries a
// network/GitHub-outage signature. It is the single grounded source of the
// network signatures #532 uses: the classifier reuses it to recognize an outage
// that surfaced through a DELIVERY error (an agent's own `gh` subprocess stderr,
// which never becomes a TransientError value), so both the typed github-owned
// path and the delivery-seam path key off exactly one signature set.
//
// The signatures are deliberately outage-specific — transport/DNS/TLS failures
// and gateway 5xx — so a deterministic 4xx (bad request, 404, 403 permission,
// 422 conflict) is never misread as retryable. Rate-limit/429 is intentionally
// NOT here: it is the runtime_quota class's job (classifyAuthQuotaStrict), and
// gh's own run() already retries a rate limit before returning.
func IsTransientMessage(text string) bool {
	l := strings.ToLower(text)
	for _, sig := range transientSignatures {
		if strings.Contains(l, sig) {
			return true
		}
	}
	return false
}

var transientSignatures = []string{
	"connection refused",
	"connection reset",
	"connection timed out",
	"could not resolve host",
	"no such host",
	"temporary failure in name resolution",
	"network is unreachable",
	"tls handshake",
	"i/o timeout",
	"timeout awaiting",
	"client.timeout exceeded",
	"context deadline exceeded",
	"unexpected eof", // gh/Go print this when the connection drops mid-response
	"read: eof",
	"http 502",
	"http 503",
	"http 504",
	"bad gateway",
	"service unavailable",
	"gateway timeout",
	"internal server error", // gh's phrasing for a 5xx api response
}

// definitiveRejectionSignatures are gh-CLI 4xx client-error signatures that prove
// the request reached GitHub and was REJECTED before it could create anything: a
// validation failure (422, e.g. a body over the 64KB issue-body limit — #738), a
// missing repo/endpoint (404), or a permission denial (403). Unlike a transient
// failure (a 5xx, a socket drop, a timeout — see transientSignatures) which MAY
// have mutated state, a definitive rejection is safe to treat as "no resource was
// created" — so a caller holding a conservative duplicate-guard latch can clear it
// and retry. gh prints these as "HTTP 422", "Validation Failed", "HTTP 404",
// "HTTP 403" (the phrasing #738's trace captured: `gh: Validation Failed (HTTP 422)`).
var definitiveRejectionSignatures = []string{
	"http 422",
	"validation failed",
	"http 404",
	"http 403",
}

// IsDefinitiveRejectionMessage reports whether a gh-CLI failure message carries a
// definitive 4xx client-rejection signature (422 validation / 404 / 403). Such a
// rejection means GitHub declined the request outright, so no issue/PR/comment was
// created — the opposite of an ambiguous transient failure (5xx/network/timeout),
// which may have mutated state before failing. A message that ALSO carries a
// transient signature is treated as ambiguous (returns false) so a conservative
// duplicate-guard latch is preserved; the two signature sets are otherwise
// disjoint (4xx vs 5xx/transport). It is the definitive-rejection counterpart of
// IsTransientMessage and, like it, keys off exactly one grounded signature set.
func IsDefinitiveRejectionMessage(text string) bool {
	if IsTransientMessage(text) {
		return false
	}
	l := strings.ToLower(text)
	for _, sig := range definitiveRejectionSignatures {
		if strings.Contains(l, sig) {
			return true
		}
	}
	return false
}

// classifyTransientError wraps a gh commandError in a TransientError when the
// command's output carries a network-outage signature, leaving every other
// failure (and its exact text) untouched. Called from GhClient.run so any gh call
// routed through it — but only the ones a caller propagates to a terminal job
// failure — carries the marker. err is the already-wrapped commandError so the
// transparent Error() stays byte-identical.
func classifyTransientError(result subprocess.Result, err error) error {
	if err == nil {
		return nil
	}
	if IsTransientMessage(result.Stderr + "\n" + result.Stdout + "\n" + err.Error()) {
		return TransientError{Err: err}
	}
	return err
}
