package cli

import (
	"fmt"
	"strings"
)

// readOnlySeatSetup is what seat setup REPORTS to its caller, distinct from
// what it refuses on. `dropped` is the pre-existing narrowing list; the
// capability fact rides beside it because it has the same shape - a fact the
// caller records or acts on - and because only a caller that will exec the
// runtime is entitled to refuse on it.
type readOnlySeatSetup struct {
	// dropped names what narrowing withheld from the staged config.
	dropped []string
	// runtimeUnavailable is why the seat's own runtime resolves to the
	// engine's exit-126 unavailable command, or "" when it staged normally.
	runtimeUnavailable string
	// toolchainUnavailable is why the seat's `go` resolves to the engine's
	// exit-126 unavailable command, or "" when the toolchain staged normally.
	toolchainUnavailable string
}

// seatRuntimeUnavailableEvent records that a job was refused because the
// runtime it would have executed is published unavailable to seats. It is a
// JOB event rather than a daemon log line because, unlike the sibling-runtime
// case, this is a fact about THIS job's outcome: the refusal is the job's
// terminal reason, and #1817's acceptance requires the failing check to be
// named where a consumer can read it without parsing English prose.
const seatRuntimeUnavailableEvent = "seat_runtime_unavailable"

// seatRuntimeCapabilityError is the #1817 capability refusal for a read-only
// seat whose OWN runtime resolves to the engine's exit-126 unavailable shim.
//
// It is a distinct type rather than a formatted error because the worker has to
// treat it differently from every other seat-setup failure: a seat-setup error
// is FAILED (something went wrong while preparing this job), while a runtime
// that cannot execute at all is BLOCKED (nothing was wrong with the job; the
// capability it needs is absent). Only a typed error can carry that
// distinction through the single error return of wrapReadOnlySandboxAdapter.
type seatRuntimeCapabilityError struct {
	agentName   string
	runtimeName string
	cause       string
}

func (e *seatRuntimeCapabilityError) Error() string {
	cause := strings.TrimSpace(e.cause)
	if cause == "" {
		cause = "no daemon-staged artifact exists"
	}
	return fmt.Sprintf(
		"read-only seat capability preflight blocked agent %q: runtime %q is published unavailable to seats, so every command this job dispatches would exit 126 without running (%s); remedy: restore the daemon-staged %s artifact, or dispatch this job to an agent whose runtime stages",
		e.agentName, e.runtimeName, cause, e.runtimeName,
	)
}

// seatToolchainUnavailableEvent records that a REVIEW job was refused because
// the seat cannot run `go`. Scoped to review for the reason the staging miss
// was left un-evented in the first place (daemon_worker.go): a job's event
// sequence must not depend on the host's toolchain and disk state, and
// TestExecBackendLocalDefaultDaemonE2E pins an exact baseline for an `ask`
// job. A review is the one action whose contract is to execute the
// repository's gate, so for review - and only review - the absence is a fact
// about the job's outcome rather than about the host.
const seatToolchainUnavailableEvent = "seat_toolchain_unavailable"

// seatToolchainCapabilityError is the #1817 capability refusal for a review
// seat whose Go toolchain is published unavailable. BLOCKED rather than
// FAILED, for the same reason as its runtime sibling: nothing was wrong with
// the job, the capability it needs is absent, and `failed` reads as "this
// reviewer tried" and invites the identical re-dispatch.
type seatToolchainCapabilityError struct {
	agentName string
	cause     string
}

func (e *seatToolchainCapabilityError) Error() string {
	cause := strings.TrimSpace(e.cause)
	if cause == "" {
		cause = "no daemon-staged Go toolchain exists"
	}
	return fmt.Sprintf(
		"read-only seat capability preflight blocked review agent %q: the seat's Go toolchain is published unavailable, so `go build`, `go vet` and `go test` would each exit 126 without running (%s); a verdict from this seat could not have executed the repository's gate; remedy: restore the daemon-staged Go artifact, or dispatch this review to a host whose toolchain stages",
		e.agentName, cause,
	)
}
