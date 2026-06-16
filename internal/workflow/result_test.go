package workflow

import (
	"strings"
	"testing"
)

func TestExtractAgentResultFromFencedOutput(t *testing.T) {
	output := "done\n```json\n" +
		`{"gitmoot_result":{"decision":"approved","summary":"looks good","findings":[],"changes_made":[],"tests_run":["go test ./..."],"needs":[],"delegations":[]}}` +
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
	if result.Findings == nil || result.ChangesMade == nil || result.TestsRun == nil || result.Needs == nil || result.Delegations == nil {
		t.Fatalf("result arrays were not normalized: %+v", result)
	}
}

func TestExtractAgentResultSkipsDecoyJSONCandidate(t *testing.T) {
	output := `{"note":"mentions gitmoot_result but is not the envelope"}` + "\n" +
		`{"gitmoot_result":{"decision":"approved","summary":"real result","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

	result, err := ExtractAgentResult(output)

	if err != nil {
		t.Fatalf("ExtractAgentResult returned error: %v", err)
	}
	if result.Summary != "real result" {
		t.Fatalf("summary = %q", result.Summary)
	}
}

func TestExtractAgentResultRejectsUnsupportedDecision(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"maybe","summary":"unclear","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`

	if _, err := ExtractAgentResult(output); err == nil {
		t.Fatal("ExtractAgentResult accepted unsupported decision")
	}
}

func TestExtractAgentResultRejectsUnsupportedField(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[],"unexpected_field":[]}}`

	_, err := ExtractAgentResult(output)

	if err == nil {
		t.Fatal("ExtractAgentResult accepted unsupported field")
	}
	if !strings.Contains(err.Error(), `unsupported gitmoot_result field "unexpected_field"`) {
		t.Fatalf("error = %v", err)
	}
}
