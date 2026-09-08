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
// MEASURED HERE, AND THE CONCLUSION WAS WRONG: the flag EXISTS on review and
// always did. `runAgentReview` routes through parseAgentRunOptions (agent.go:482),
// the same parser implement uses, so the flag is parsed and honoured. Only the
// review verb's USAGE LINE omitted it, which is a discoverability defect rather
// than a missing capability - a much cheaper thing to be wrong about, and worth
// pinning in both directions so nobody "adds" a flag that is already there.

// THE BEHAVIOUR ARM, which is the one that matters: review must ACCEPT the flag.
// Asserted through the real argument parser with a positive control, because an
// error is not evidence of rejection by itself - every one of these probes
// errors, and only the reason distinguishes them.
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

// And the flag must actually reach the option, not merely be tolerated: a parser
// that swallowed it silently would pass the arm above while the fanout still ran.
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
		t.Fatal("the flag parsed but did not set skipNativeReviewFanout, so the fanout would still run")
	}
	// The default must stay OFF, or the fix would silently disable fanout for
	// every review that never asked.
	def, ok := parseAgentRunOptions("review", []string{
		"some-agent", "review this", "--repo", "owner/repo", "--pr", "7",
	}, &stderr)
	if !ok {
		t.Fatalf("parse failed without the flag: %q", stderr.String())
	}
	if def.skipNativeReviewFanout {
		t.Fatal("skipNativeReviewFanout defaults to true; native fanout would be off for every review")
	}
}

// The DISCOVERABILITY arm, which is what #1654 actually found. Asserting the one
// token rather than the whole usage sentence: pinning the sentence would be
// pinning prose, which this fleet forbids.
func TestAgentReviewUsageDocumentsTheFanoutFlag(t *testing.T) {
	for _, command := range []string{"review", "implement", "run", "orchestrate"} {
		var out bytes.Buffer
		printAgentRunUsage(&out, command)
		if !strings.Contains(out.String(), "--skip-native-review-fanout") {
			t.Fatalf("%s usage omits --skip-native-review-fanout, so an operator cannot find the escape hatch:\n%s", command, out.String())
		}
	}
}
