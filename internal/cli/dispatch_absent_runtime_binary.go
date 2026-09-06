package cli

import (
	"context"
	"reflect"

	"github.com/gitmoot/gitmoot/internal/db"
	"github.com/gitmoot/gitmoot/internal/execbackend"
	"github.com/gitmoot/gitmoot/internal/runtime"
	"github.com/gitmoot/gitmoot/internal/workflow"
)

// refuseDispatchOnAbsentRuntimeBinary is the #1817 dispatch predicate settled by
// ruling 123815. It refuses ONLY when all three facts hold:
//
//  1. execsDeclaredBinary - the caller states, explicitly, that this dispatch
//     builds a real adapter which will exec the runtime's declared CLI binary.
//     Never inferred here; supplied by the dispatch entry.
//  2. the execution backend runs on THIS host, decided by execbackend.Consume
//     exactly as the existing contract preflight decides it. A remote or
//     attached instance is somebody else's PATH, so a local probe would answer
//     about the wrong machine.
//  3. the runtime's declared executable does not resolve.
//
// If any is false the dispatch proceeds untouched. That ordering is the whole
// correction: the first head keyed refusal on absence alone and blocked 24
// internal/cli tests whose dispatches deliver through an INJECTED adapter that
// never execs the binary; the second keyed it on contract state and never
// refused at all, because absence is honestly classified as unknown.
//
// The unsafe direction is refusal, so every uncertain input proceeds: an
// unknown-state contract, a missing preflight hook, a remote backend, or a
// caller that never declared execsDeclaredBinary.
func refuseDispatchOnAbsentRuntimeBinary(ctx context.Context, backend execbackend.Backend, agent runtime.Agent, execsDeclaredBinary bool) error {
	if !execsDeclaredBinary {
		return nil
	}
	// Probes through localRuntimeContractPreflight, the SAME seam the foreground
	// contract gate uses, so there is one probe path and one test override rather
	// than a second opinion that could drift from it.
	result, checked, err := runtimeContractPreflightForBackend(backend, func() runtime.RuntimeContractResult {
		return localRuntimeContractPreflight(ctx, agent)
	})
	if err != nil {
		return err
	}
	// NO SEPARATE REMOTE GUARD, and its absence is deliberate rather than an
	// oversight. runtimeContractPreflightForBackend routes through
	// execbackend.Consume, whose REMOTE branch never calls the local probe and
	// returns a zero result - so there are no requirements to find and nothing to
	// refuse. A `if !checked { return nil }` line here reads like the safeguard
	// but cannot fail: measured, a mutant disabling it changed no test, because
	// the empty result already decides. The exemption is pinned instead by
	// asserting the local probe is never invoked on a remote backend.
	_ = checked
	return runtime.RuntimeContractAbsentBinaryError(agent, result)
}

// dispatchLocalAgentJobFromCLI is the PRODUCTION entry to local agent dispatch.
// It exists so the #1817 real-adapter declaration lives in exactly ONE place:
// every production caller routes through here, and tests call
// dispatchLocalAgentJob directly, which is why an injected adapter is
// dispatchable by omission.
//
// Measured reason for the wrapper rather than two struct literals: with the
// declaration written at each production site, a mutant that flipped ONE of
// them to false survived the whole suite, because the tests supply the field
// themselves. One declaration point is one thing to forget, and it is pinned.
func dispatchLocalAgentJobFromCLI(ctx context.Context, store *db.Store, request localAgentDispatchRequest) (localAgentJobOutput, error) {
	request.ExecsDeclaredBinary = cliDispatchExecsDeclaredBinary
	return dispatchLocalAgentJob(ctx, store, request)
}

// cliDispatchExecsDeclaredBinary is the process-level declaration that a CLI
// dispatch will build a real adapter and exec its runtime's declared binary.
// True in production; the same idiom this package already uses for
// localRuntimeContractPreflight, localAgentDispatchRuntimeAdapterFor and
// localAgentDispatchExecBackendFor.
//
// IT EXISTS BECAUSE ENTERING THROUGH THE CLI ENTRY DOES NOT BY ITSELF MEAN THE
// BINARY WILL BE EXEC'D, and that was measured rather than reasoned: tests that
// drive `agent review` and the managed-agent paths legitimately do so with NO
// runtime installed, because their subject is dispatch bookkeeping - requeue on
// a busy session, managed instance restarts - and delivery is either injected or
// never reached. Declaring on their behalf refused three of them on a runner
// with no CLIs. A test that is not going to exec says so here, explicitly,
// which is what "caller-supplied" has to mean when the caller is a command
// rather than a struct literal.
var cliDispatchExecsDeclaredBinary = true

// setRealAdapterFactory records a delivery adapter factory AND the fact that it
// builds a real adapter which execs its runtime's declared CLI binary
// (#1926-f5).
//
// It stores the factory's CODE POINTER, not merely a boolean, and that is the
// load-bearing part. AdapterFactory is an exported plain field that ~22 test
// fixtures assign directly to inject a fake; a bare boolean set by the
// constructor would survive that assignment and refuse an injected adapter for
// an absence it will never reach. Measured: it did exactly that on the first
// attempt. Binding the claim to the function it was made ABOUT means replacing
// the factory silently withdraws the claim, which is the behaviour the injected
// exemption requires and needs no edit to any fixture.
func (w *jobWorker) setRealAdapterFactory(factory func(runtime.Agent, string) (workflow.DeliveryAdapter, error)) {
	w.AdapterFactory = factory
	w.adapterExecsDeclaredBinary = true
	w.declaredRealAdapterPtr = reflect.ValueOf(factory).Pointer()
}

// adapterIsDeclaredReal reports whether the factory currently installed is the
// one that was declared to exec its runtime's declared binary.
func (w jobWorker) adapterIsDeclaredReal() bool {
	if !w.adapterExecsDeclaredBinary || w.AdapterFactory == nil || w.declaredRealAdapterPtr == 0 {
		return false
	}
	return reflect.ValueOf(w.AdapterFactory).Pointer() == w.declaredRealAdapterPtr
}
