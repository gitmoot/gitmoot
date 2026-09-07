package runtime

import (
	"context"
	"strings"
	"testing"
)

// #1817: AN ABSENT RUNTIME EXECUTABLE MUST BE NAMED AS ITSELF, AND MUST NOT
// BLOCK A DISPATCH.
//
// The measured failure this makes legible:
//
//	sandbox-exec: resolve sandbox target "claude": exec: "claude": executable
//	file not found in $PATH
//
// which killed two #1910 review lens legs on 2026-09-05 17:28 and two joltra
// review jobs on 2026-09-06 at 10:51:15 and 11:06:20. Before this change the
// probe reported that absence as "binary help output was not parseable", once
// per declared flag, so the one actionable fact was both mis-described and
// duplicated.
//
// WHY IT DOES NOT BLOCK, measured rather than assumed. I first classified
// absence as unsupported so dispatch would refuse it, and CI answered: 24
// internal/cli tests went `blocked, want succeeded/failed/queued`, because the
// runner has no runtime CLIs installed while those dispatches deliver through an
// injected adapter that never execs the declared binary. Every pre-existing
// `unsupported` case presupposes a PRESENT binary whose help was parsed. This
// layer cannot know whether the real adapter will exec that binary on this host,
// so refusing here rejects valid input.
func TestMissingBinaryIsNamedAsAbsentAndDoesNotBlock(t *testing.T) {
	// path "" makes the harness LookPath fail, which is what an absent CLI does.
	checker := NewRuntimeContractChecker(&contractProbeRunner{}, BuiltinRuntimeRegistry())
	agent := Agent{Name: "gm-review-claude", Runtime: ClaudeRuntime, AutonomyPolicy: AutonomyPolicyAuto}

	result := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})

	// UNKNOWN, not unsupported: the tri-state's own contract is that only
	// unsupported blocks, and absence is not an answer this layer may act on.
	if result.State != RuntimeContractUnknown {
		t.Fatalf("state = %q, want %q", result.State, RuntimeContractUnknown)
	}
	if err := RuntimeContractDispatchError(agent, result); err != nil {
		t.Fatalf("an absent executable blocked a dispatch this layer cannot judge: %v", err)
	}

	// EXACTLY ONE requirement row describes the absence, and it is its own kind.
	// Claude declares two flag requirements and omp declares six; reporting
	// absence per-flag both multiplies the row and mis-names the defect.
	var absence []RuntimeRequirementResult
	for _, requirement := range result.Requirements {
		if requirement.Kind == RuntimeRequirementBinaryPresent {
			absence = append(absence, requirement)
		}
	}
	if len(absence) != 1 {
		t.Fatalf("binary-present rows = %d, want exactly 1 (requirements=%+v)", len(absence), result.Requirements)
	}
	// No flag row may be emitted at all: a flag verdict about a binary that does
	// not exist is a statement about nothing.
	for _, requirement := range result.Requirements {
		if requirement.Kind == RuntimeRequirementFlag {
			t.Fatalf("a flag requirement was judged against an absent binary: %+v", requirement)
		}
	}

	// The row must NAME the executable and carry the resolver's own error, or an
	// operator reading `runtime inspect` still cannot tell what to install.
	row := absence[0]
	for _, want := range []string{`executable "claude"`} {
		if !strings.Contains(row.Name, want) {
			t.Fatalf("requirement name %q does not contain %q", row.Name, want)
		}
	}
	if !strings.Contains(row.Detail, "claude") {
		t.Fatalf("requirement detail %q does not carry the resolver error naming the binary", row.Detail)
	}
	if !strings.Contains(row.Remedy, "install claude") {
		t.Fatalf("requirement remedy %q does not say what to install", row.Remedy)
	}
	// And it must NOT be described as an installed version failing a check.
	if strings.Contains(row.Detail, "not parseable") {
		t.Fatalf("absence is still being reported as an unparseable help response: %q", row.Detail)
	}
}

// CONTROL: a binary that EXISTS whose help cannot be parsed is a different fact
// and must keep its existing shape - unknown, non-blocking, flag rows present,
// and NO binary-present row. Without this, a mutant that reported every probe as
// absence would satisfy the test above.
func TestPresentBinaryWithUnparseableHelpKeepsFlagRows(t *testing.T) {
	checker, _, path := newContractCheckerForTest(t, "unparseable")
	agent := Agent{Name: "seat", Runtime: KimiRuntime, AutonomyPolicy: AutonomyPolicyAuto}

	result := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})

	if result.State != RuntimeContractUnknown {
		t.Fatalf("state = %q, want %q", result.State, RuntimeContractUnknown)
	}
	if result.ResolvedPath != path {
		t.Fatalf("resolved path = %q, want %q", result.ResolvedPath, path)
	}
	for _, requirement := range result.Requirements {
		if requirement.Kind == RuntimeRequirementBinaryPresent {
			t.Fatalf("a present binary produced an absence row: %+v", requirement)
		}
	}
	flags := 0
	for _, requirement := range result.Requirements {
		if requirement.Kind == RuntimeRequirementFlag {
			flags++
		}
	}
	if flags == 0 {
		t.Fatal("a present binary produced no flag requirement rows, so the existing probe path was skipped")
	}
	if err := RuntimeContractDispatchError(agent, result); err != nil {
		t.Fatalf("an unparseable help response blocked a dispatch: %v", err)
	}
}

// A genuinely unsupported runtime - present binary, help parsed, required flag
// absent - must STILL block. This is the arm that proves the change did not
// disarm the gate while making absence quieter.
func TestParsedHelpMissingRequiredFlagStillBlocks(t *testing.T) {
	checker, _, _ := newContractCheckerForTest(t, "unsupported")
	agent := Agent{Name: "seat", Runtime: KimiRuntime, AutonomyPolicy: AutonomyPolicyAuto}

	result := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})

	if result.State != RuntimeContractUnsupported {
		t.Fatalf("state = %q, want %q", result.State, RuntimeContractUnsupported)
	}
	if err := RuntimeContractDispatchError(agent, result); err == nil {
		t.Fatal("a parsed help response missing a required flag no longer blocks dispatch")
	}
}

// The shell runtime declares no contract and no binary, so it must produce no
// absence row and must not block.
func TestContractlessRuntimeProducesNoAbsenceRow(t *testing.T) {
	checker := NewRuntimeContractChecker(&contractProbeRunner{}, BuiltinRuntimeRegistry())
	agent := Agent{Name: "shell-seat", Runtime: ShellRuntime, AutonomyPolicy: AutonomyPolicyAuto}

	result := checker.CheckRequest(context.Background(), agent, RuntimeContractRequest{})

	if result.State != RuntimeContractSupported {
		t.Fatalf("state = %q, want %q", result.State, RuntimeContractSupported)
	}
	for _, requirement := range result.Requirements {
		if requirement.Kind == RuntimeRequirementBinaryPresent {
			t.Fatalf("a contractless runtime produced an absence row: %+v", requirement)
		}
	}
	if err := RuntimeContractDispatchError(agent, result); err != nil {
		t.Fatalf("contractless runtime was blocked: %v", err)
	}
}
