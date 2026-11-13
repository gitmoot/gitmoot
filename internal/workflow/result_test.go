package workflow

import (
	"strings"
	"testing"
)

func TestExtractAgentResultFromFencedOutput(t *testing.T) {
	output := "done\n```json\n" +
		`{"gitmoot_result":{"decision":"approved","summary":"looks good","findings":[],"changes_made":[],"tests_run":["go test ./..."],"needs":[]}}` +
		"\n```"

	result, err := ExtractAgentResult(output)

	if err != nil {
		t.Fatalf("ExtractAgentResult returned error: %v", err)
	}
	if result.Decision != "approved" || result.Summary != "looks good" {
		t.Fatalf("result = %+v", result)
	}
	if len(result.TestsRun) != 1 || result.TestsRun[0] != "go test ./..." {
		t.Fatalf("tests_run = %+v", result.TestsRun)
	}
}

func TestExtractAgentResultRejectsMalformedOutput(t *testing.T) {
	if _, err := ExtractAgentResult("plain text without contract"); err == nil {
		t.Fatal("ExtractAgentResult accepted output without gitmoot_result")
	}
}

func TestExtractAgentResultNormalizesMissingArrays(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"approved","summary":"minimal"}}`

	result, err := ExtractAgentResult(output)

	if err != nil {
		t.Fatalf("ExtractAgentResult returned error: %v", err)
	}
	if result.Findings == nil || result.ChangesMade == nil || result.TestsRun == nil || result.Needs == nil {
		t.Fatalf("result arrays were not normalized: %+v", result)
	}
}

func TestExtractAgentResultSkipsDecoyJSONCandidate(t *testing.T) {
	output := `{"note":"mentions gitmoot_result but is not the envelope"}` + "\n" +
		`{"gitmoot_result":{"decision":"approved","summary":"real result","findings":[],"changes_made":[],"tests_run":[],"needs":[]}}`

	result, err := ExtractAgentResult(output)

	if err != nil {
		t.Fatalf("ExtractAgentResult returned error: %v", err)
	}
	if result.Summary != "real result" {
		t.Fatalf("summary = %q", result.Summary)
	}
}

func TestExtractAgentResultRejectsUnsupportedDecision(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"maybe","summary":"unclear","findings":[],"changes_made":[],"tests_run":[],"needs":[]}}`

	if _, err := ExtractAgentResult(output); err == nil {
		t.Fatal("ExtractAgentResult accepted unsupported decision")
	}
}

func TestExtractAgentResultAcceptsCanonicalReviewSeverities(t *testing.T) {
	for _, severity := range ReviewSeverities {
		t.Run(severity, func(t *testing.T) {
			output := `{"gitmoot_result":{"decision":"changes_requested","severity":"` + severity + `","summary":"fix finding"}}`

			result, err := extractAgentResultForAction(output, "review")
			if err != nil {
				t.Fatalf("extractAgentResultForAction returned error: %v", err)
			}
			if result.Severity != severity {
				t.Fatalf("severity = %q, want %q", result.Severity, severity)
			}
		})
	}
}

func TestExtractAgentResultRequiresSeverityForChangesRequestedReview(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"changes_requested","summary":"fix finding"}}`

	_, err := extractAgentResultForAction(output, "review")
	if err == nil {
		t.Fatal("extractAgentResultForAction accepted a review changes_requested verdict without severity")
	}
	if !strings.Contains(err.Error(), "severity is required when a review requests changes") {
		t.Fatalf("error = %v", err)
	}

	if _, err := extractAgentResultForAction(output, "implement"); err != nil {
		t.Fatalf("non-review changes_requested result rejected: %v", err)
	}
}

func TestExtractAgentResultRejectsUnsupportedReviewSeverity(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"changes_requested","severity":"critical","summary":"fix finding"}}`

	_, err := extractAgentResultForAction(output, "review")
	if err == nil {
		t.Fatal("extractAgentResultForAction accepted unsupported severity")
	}
	if !strings.Contains(err.Error(), `unsupported gitmoot_result severity "critical"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestExtractAgentResultAcceptsSkipped(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"skipped","summary":"no new replies","findings":[],"changes_made":[],"tests_run":[],"needs":[]}}`

	result, err := ExtractAgentResult(output)
	if err != nil {
		t.Fatalf("ExtractAgentResult returned error: %v", err)
	}
	if result.Decision != "skipped" || result.Summary != "no new replies" {
		t.Fatalf("result = %+v", result)
	}
}

func TestExtractAgentResultRejectsUnsupportedField(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"unexpected_field":[]}}`

	_, err := ExtractAgentResult(output)

	if err == nil {
		t.Fatal("ExtractAgentResult accepted unsupported field")
	}
	if !strings.Contains(err.Error(), `unsupported gitmoot_result field "unexpected_field"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestExtractAgentResultRejectsNextAgentsField(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"next_agents":["impl"]}}`

	_, err := ExtractAgentResult(output)

	if err == nil {
		t.Fatal("ExtractAgentResult accepted next_agents field")
	}
	if !strings.Contains(err.Error(), `unsupported gitmoot_result field "next_agents"`) {
		t.Fatalf("error = %v", err)
	}
}

// TestRemovedResultFieldsAreRejectedNotIgnored pins a DELIBERATE consequence of
// #2202 rather than an accident of it. `learnings` was removed from AgentResult,
// and because agentResultAllowedFields is derived from the struct's JSON tags,
// an agent that still emits it now fails the whole job with
// `unsupported gitmoot_result field "learnings"` instead of having the field
// dropped. 133 review jobs emitted it in the fortnight before removal, so this
// is a real behaviour change for real callers and the round-1 reviewer was right
// to say the PR should name it.
//
// It stays strict on purpose. The roster is the mechanism that catches a typo or
// a hallucinated field before it becomes a silent no-op, and carving a
// tolerated-but-ignored exception for a removed field is a permanent shim of
// exactly the kind this campaign exists to delete. The exposure is also bounded
// by construction: the prompt no longer asks for the field, and the prompt is
// rendered per job, so no job dispatched after this ships is told to send it.
// Same shape as the next_agents removal in b25986cc.
func TestRemovedResultFieldsAreRejectedNotIgnored(t *testing.T) {
	for _, field := range []string{"learnings", "next_agents"} {
		raw := `{"gitmoot_result":{"decision":"approved","summary":"s","evidence":"executed","` + field + `":[{"key":"k","content":"c"}]}}`
		_, err := ExtractAgentResult(raw)
		if err == nil {
			t.Fatalf("result carrying removed field %q was accepted; a removed field must fail loudly, not be silently dropped", field)
		}
		if got := err.Error(); !strings.Contains(got, field) {
			t.Fatalf("error for removed field %q = %q; it must name the offending field so the emitter can be fixed", field, got)
		}
	}
	// Control: the same envelope without the removed field must pass, so the test
	// is not merely asserting that everything fails.
	if _, err := ExtractAgentResult(`{"gitmoot_result":{"decision":"approved","summary":"s","evidence":"executed"}}`); err != nil {
		t.Fatalf("clean result rejected: %v", err)
	}
}
