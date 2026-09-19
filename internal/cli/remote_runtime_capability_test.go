package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
)

// TestRemoteCapableRuntimeIsTheOnlyAllowlist pins the property that made
// centralising worth doing: DISPATCH VALIDATION AND GATEWAY ADMISSION MUST
// AGREE FOR EVERY RUNTIME.
//
// Before #2234's review the predicate was written out three times — in
// validateRuntimeExecutionBackend, provisionRemoteCredentialGateway, and
// provisionExecutionBackend. Three lists answering one question, and nothing
// failed when only one changed.
//
// The drift directions are asymmetric, which is why this is a test rather than
// a convention. If the gateway admits a runtime dispatch rejects, a supported
// combination is refused loudly at dispatch — annoying and visible. If dispatch
// admits one the gateway rejects, the job PASSES DISPATCH AND DIES AFTER COST
// RESERVATION AND PROVISIONING, which is the exact outcome #2234's
// refuse-before-reserve requirement exists to prevent. The dangerous direction
// is the one that looks like success at the point the check was added.
//
// This is the test that fires when someone adds a third remote-capable runtime
// — the change #1543 and #1633 both point at — and wires it into one site.
func TestRemoteCapableRuntimeIsTheOnlyAllowlist(t *testing.T) {
	t.Parallel()

	// Every runtime the fleet defines, not only the ones expected to pass. A
	// table containing only supported runtimes would agree with any predicate.
	runtimes := []string{
		runtime.ShellRuntime,
		runtime.OmpRuntime,
		runtime.ClaudeRuntime,
		runtime.CodexRuntime,
		runtime.KimiRuntime,
		"",
		"  ",
		"not-a-runtime",
	}

	for _, runtimeName := range runtimes {
		supported := slices.Contains(remoteCapableRuntimes[:], strings.TrimSpace(runtimeName))

		dispatchErr := validateRuntimeExecutionBackend(runtimeName, execbackend.Remote)
		if supported && dispatchErr != nil {
			t.Fatalf("runtime %q is remote-capable but dispatch validation refused it: %v", runtimeName, dispatchErr)
		}
		if !supported && dispatchErr == nil {
			t.Fatalf("runtime %q is not remote-capable but dispatch validation admitted it; such a job would fail only after reserve and provision", runtimeName)
		}

		// The gateway's runtime gate runs before it needs a configured plan, so a
		// zero plan exercises the real admission check on the production path.
		_, _, gatewayErr := jobWorker{}.provisionRemoteCredentialGateway(
			context.Background(), execbackend.Remote, runtimeName, "job-1", 0,
			remoteCredentialGatewayPlan{}, nil, nil,
		)
		if supported && gatewayErr != nil {
			t.Fatalf("runtime %q is remote-capable but the credential gateway refused it: %v", runtimeName, gatewayErr)
		}
		if !supported && gatewayErr == nil {
			t.Fatalf("runtime %q is not remote-capable but the credential gateway admitted it", runtimeName)
		}

		// The load-bearing assertion: the two answers agree. Either both admit or
		// both refuse — a disagreement is the drift this centralisation removes.
		if (dispatchErr == nil) != (gatewayErr == nil) {
			t.Fatalf("runtime %q: dispatch and gateway disagree (dispatch err=%v, gateway err=%v)", runtimeName, dispatchErr, gatewayErr)
		}
	}
}

// TestRemoteCapableRuntimeRefusalNamesTheSupportedSet keeps the operator-visible
// list tied to the predicate that enforces it. A refusal that names a set the
// code does not implement is worse than one that names none: it is confidently
// wrong, and the operator acts on it.
func TestRemoteCapableRuntimeRefusalNamesTheSupportedSet(t *testing.T) {
	t.Parallel()

	err := validateRuntimeExecutionBackend(runtime.ClaudeRuntime, execbackend.Remote)
	if err == nil {
		t.Fatal("expected claude to be refused on the remote backend")
	}
	rendered := remoteCapableRuntimeNames()
	for _, runtimeName := range runtime.SupportedRuntimes() {
		expected := slices.Contains(remoteCapableRuntimes[:], runtimeName)
		if actual := remoteCapableRuntime(runtimeName); actual != expected {
			t.Fatalf("runtime %q: capability predicate=%v, canonical list=%v", runtimeName, actual, expected)
		}
		if named := strings.Contains(rendered, runtimeName); named != expected {
			t.Fatalf("runtime %q: rendered names %q membership=%v, canonical list=%v", runtimeName, rendered, named, expected)
		}
	}
	if !strings.Contains(err.Error(), rendered) {
		t.Fatalf("refusal %q does not include canonical supported set %q", err.Error(), rendered)
	}
	if !strings.Contains(err.Error(), runtime.ClaudeRuntime) {
		t.Fatalf("refusal %q does not name the refused runtime, so the operator cannot tell which job it applies to", err.Error())
	}
}
