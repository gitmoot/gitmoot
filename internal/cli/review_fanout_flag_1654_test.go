package cli

import (
	"bytes"
	"strings"
	"testing"
)

// #1654. The issue measured `agent review --help | grep -c
// skip-native-review-fanout` = 0 against 1 for implement, and concluded there is
// "no way to suppress it, because the escape hatch does not exist on the verb
// that creates the hazard."
//
// THE CONCLUSION WAS WRONG, AND SO WAS THE FIRST CORRECTION. The flag is parsed
// on review and always was: `runAgentReview` routes through
// parseAgentRunOptions, the same parser implement uses. The first version of
// this file called that "parsed and honoured" and framed the flag as a review
// escape hatch. Parsed is true; honoured is not.
//
// A REAL, WORKING CONTROL ON THE WRONG VERB. In
// internal/workflow/engine_run_budgets.go the flag is read only inside
// `case "implement":`, where it writes the branch lock and rides onto the
// PR-open event. `case "review":` begins after that block and never consults it,
// and dispatchFix builds a parentless implement request that does not inherit
// it. So passing it to `agent review` records a payload bit nothing consumes,
// and it does not prevent the per-review fix-job race.
//
// The tests below therefore pin PARSING and DISCOVERABILITY only, which is all
// the CLI layer decides. What the engine does with the bit is pinned in
// internal/workflow, beside the code that makes the decision.

// Review must ACCEPT the flag: it shares implement's parser, and a verb that
// rejected it would break the shared-parser invariant.
func TestAgentReviewAcceptsSkipNativeReviewFanout(t *testing.T) {
	var stderr bytes.Buffer
	_, ok := parseAgentRunOptions("review", []string{
		"some-agent", "review this", "--repo", "owner/repo", "--pr", "7",
		"--skip-native-review-fanout",
	}, &stderr)
	if !ok {
		t.Fatalf("review rejected --skip-native-review-fanout: %q", stderr.String())
	}

	// POSITIVE CONTROL: the same parser on the same verb must reject a flag that
	// really is unknown. Without this, "ok" above could mean the parser accepts
	// anything.
	stderr.Reset()
	if _, ok := parseAgentRunOptions("review", []string{
		"some-agent", "review this", "--repo", "owner/repo", "--pr", "7",
		"--definitely-not-a-flag",
	}, &stderr); ok {
		t.Fatal("review accepted an unknown flag, so the acceptance above proves nothing")
	}
	if !strings.Contains(stderr.String(), "unknown agent review flag") {
		t.Fatalf("control rejected for the wrong reason: %q", stderr.String())
	}
}

// The parsed value must reach the option rather than being silently swallowed.
// This is a claim about the PARSER only: it says the bit is set, and says
// nothing about any consumer acting on it.
func TestAgentReviewFanoutFlagReachesTheOption(t *testing.T) {
	var stderr bytes.Buffer
	options, ok := parseAgentRunOptions("review", []string{
		"some-agent", "review this", "--repo", "owner/repo", "--pr", "7",
		"--skip-native-review-fanout",
	}, &stderr)
	if !ok {
		t.Fatalf("parse failed: %q", stderr.String())
	}
	if !options.skipNativeReviewFanout {
		t.Fatal("the flag parsed but did not set skipNativeReviewFanout")
	}
	// The default must stay OFF, or the implement path would silently lose its
	// native fan-out for every dispatch that never asked.
	def, ok := parseAgentRunOptions("review", []string{
		"some-agent", "review this", "--repo", "owner/repo", "--pr", "7",
	}, &stderr)
	if !ok {
		t.Fatalf("parse failed without the flag: %q", stderr.String())
	}
	if def.skipNativeReviewFanout {
		t.Fatal("skipNativeReviewFanout defaults to true; native fanout would be off by default")
	}
}

// The DISCOVERABILITY arm, which is what #1654 actually found. Asserting the one
// token rather than the whole usage sentence: pinning the sentence would be
// pinning prose.
func TestAgentReviewUsageDocumentsTheFanoutFlag(t *testing.T) {
	for _, command := range []string{"review", "implement", "run", "orchestrate"} {
		var out bytes.Buffer
		printAgentRunUsage(&out, command)
		if !strings.Contains(out.String(), "--skip-native-review-fanout") {
			t.Fatalf("%s usage omits --skip-native-review-fanout, so an operator cannot find it:\n%s", command, out.String())
		}
	}
}
