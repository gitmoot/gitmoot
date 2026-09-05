package runtime

import (
	"context"
	"strings"
	"testing"
)

// #1817: A RUNTIME WHOSE EXECUTABLE DOES NOT EXIST MUST NOT BE DISPATCHED.
//
// Measured instance this pins, from 2026-09-05 17:28: two #1910 review lens legs
// were dispatched and died about fifteen seconds later with
//
//	sandbox-exec: resolve sandbox target "claude": exec: "claude": executable
//	file not found in $PATH
//
// The preflight had already run and had not blocked them. probeBinary resolves
// the runtime binary with the same exec.LookPath the sandbox target uses
// (internal/sandbox/lookpath.go), so both sides agree the binary is absent - but
// a LookPath failure produced helpParsed=false, every flag requirement became
// RuntimeContractUnknown, and unknown is documented to MUST RUN. So the most
// DEFINITIVE answer the probe can obtain was classified as no answer at all.
//
// The pair below is the whole point and must be read together: absence blocks,
// and everything that is merely unestablished still runs.
func TestRuntimePreflightMissingBinaryBlocksDispatch(t *testing.T) {
	// path "" makes the harness LookPath fail, which is what an absent CLI does.
	checker := NewRuntimeContractChecker(&contractProbeRunner{}, BuiltinRuntimeRegistry())
	agent := Agent{Name: "gm-review-claude", Runtime: ClaudeRuntime, AutonomyPolicy: AutonomyPolicyAuto}

	result := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})

	if result.State != RuntimeContractUnsupported {
		t.Fatalf("state = %q, want %q; an executable that cannot be resolved is a definitive answer, not an unestablished one, and %q lets the leg dispatch and die at exec time",
			result.State, RuntimeContractUnsupported, RuntimeContractUnknown)
	}
	err := RuntimeContractDispatchError(agent, result)
	if err == nil {
		t.Fatal("dispatch was not blocked for a runtime whose executable does not resolve")
	}
	// Fail loudly with a NAMED cause. Asserted as SHAPE, not as token presence,
	// and that distinction is measured: ClaudeRuntime, the contract binary and
	// this agent's name all contain the substring "claude", so a
	// strings.Contains(err, "claude") triple is one assertion wearing three hats
	// and a mutant that dropped the runtime field entirely survived it. These
	// fragments cannot all be produced by the probe detail alone.
	for _, want := range []string{
		`agent "gm-review-claude"`,
		`runtime "claude"`,
		`executable "claude"`,
		"does not resolve on PATH",
		"install claude on the dispatching host PATH",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("dispatch error is missing %q, so the refusal does not name its cause: %v", want, err)
		}
	}
	// And it must NOT claim an installed version failed a requirement: nothing is
	// installed, and that wording sends the reader after a CLI upgrade.
	if strings.Contains(err.Error(), "installed version") {
		t.Fatalf("refusal describes an absent binary as an installed version: %v", err)
	}
}

// POSITIVE CONTROL, and it is the guard that matters most on this change: every
// bound added to this repo lately has had a version that rejected valid input.
// A binary that EXISTS whose help cannot be parsed is genuinely unestablished -
// the CLI has not said no - so it must stay unknown and must still dispatch.
// If this test ever fails, the change has started refusing runnable reviewers.
func TestRuntimePreflightPresentBinaryWithUnparseableHelpStillDispatches(t *testing.T) {
	checker, _, path := newContractCheckerForTest(t, "unparseable")
	agent := Agent{Name: "seat", Runtime: KimiRuntime, AutonomyPolicy: AutonomyPolicyAuto}

	result := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})

	if result.State != RuntimeContractUnknown {
		t.Fatalf("state = %q, want %q for a present binary whose help is unparseable", result.State, RuntimeContractUnknown)
	}
	if result.ResolvedPath != path {
		t.Fatalf("resolved path = %q, want %q; the probe must still report where it found the binary", result.ResolvedPath, path)
	}
	if err := RuntimeContractDispatchError(agent, result); err != nil {
		t.Fatalf("an unparseable help response blocked a dispatch it must not block: %v", err)
	}
}

// The shell runtime declares no contract and therefore no binary. It must be
// untouched by a binary-resolution rule, or subscribing a shell agent starts
// failing preflight on a binary it never names.
func TestRuntimePreflightContractlessRuntimeIsUnaffected(t *testing.T) {
	checker := NewRuntimeContractChecker(&contractProbeRunner{}, BuiltinRuntimeRegistry())
	agent := Agent{Name: "shell-seat", Runtime: ShellRuntime, AutonomyPolicy: AutonomyPolicyAuto}

	result := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})

	if result.State != RuntimeContractSupported {
		t.Fatalf("state = %q, want %q for a runtime that declares no contract", result.State, RuntimeContractSupported)
	}
	if err := RuntimeContractDispatchError(agent, result); err != nil {
		t.Fatalf("contractless runtime was blocked: %v", err)
	}
}
