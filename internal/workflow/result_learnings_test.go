package workflow

import (
	"encoding/json"
	"testing"
)

// TestExtractAgentResultAbsentLearningsIsBackwardCompatible proves a result that
// omits the #626 learnings field parses exactly as before: no learnings, no
// error. This is the byte-identical-when-off backward-compat guarantee.
func TestExtractAgentResultAbsentLearningsIsBackwardCompatible(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"approved","summary":"ok","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[]}}`
	result, err := ExtractAgentResult(output)
	if err != nil {
		t.Fatalf("ExtractAgentResult returned error: %v", err)
	}
	if len(result.Learnings) != 0 {
		t.Fatalf("absent learnings should parse to empty, got %+v", result.Learnings)
	}
	// A round-trip marshal must NOT emit a learnings key (omitempty), so a result
	// that never touched memory serializes byte-identically.
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(encoded); containsKey(got, "learnings") {
		t.Fatalf("omitted learnings must not serialize a key, got %s", got)
	}
}

// TestExtractAgentResultAcceptsLearnings proves a well-formed learnings array
// parses into typed entries with scope defaulting handled by the caller.
func TestExtractAgentResultAcceptsLearnings(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"implemented","summary":"done","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[],` +
		`"learnings":[{"key":"ci-flake","scope":"repo","content":"arm64 CI is flaky"},{"key":"race-timeout","scope":"general","content":"race suites need a long timeout"}]}}`
	result, err := ExtractAgentResult(output)
	if err != nil {
		t.Fatalf("ExtractAgentResult returned error: %v", err)
	}
	if len(result.Learnings) != 2 {
		t.Fatalf("want 2 learnings, got %d", len(result.Learnings))
	}
	if result.Learnings[0].Key != "ci-flake" || result.Learnings[0].Scope != "repo" || result.Learnings[0].Content != "arm64 CI is flaky" {
		t.Fatalf("learnings[0] = %+v", result.Learnings[0])
	}
	if result.Learnings[1].Scope != "general" {
		t.Fatalf("learnings[1] scope = %q", result.Learnings[1].Scope)
	}
}

// TestExtractAgentResultAcceptsLearningWithoutScope proves scope is optional
// (empty is accepted; the store defaults it to repo).
func TestExtractAgentResultAcceptsLearningWithoutScope(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"approved","summary":"s","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[],` +
		`"learnings":[{"key":"k","content":"a durable fact"}]}}`
	result, err := ExtractAgentResult(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Learnings) != 1 || result.Learnings[0].Scope != "" {
		t.Fatalf("want one scopeless learning, got %+v", result.Learnings)
	}
}

func TestExtractAgentResultRejectsMalformedLearnings(t *testing.T) {
	cases := map[string]string{
		"missing key": `{"gitmoot_result":{"decision":"approved","summary":"s","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[],"learnings":[{"content":"no key"}]}}`,
		"blank content": `{"gitmoot_result":{"decision":"approved","summary":"s","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[],"learnings":[{"key":"k","content":"   "}]}}`,
		"bad scope":     `{"gitmoot_result":{"decision":"approved","summary":"s","findings":[],"changes_made":[],"tests_run":[],"needs":[],"delegations":[],"learnings":[{"key":"k","scope":"planetary","content":"c"}]}}`,
	}
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ExtractAgentResult(output); err == nil {
				t.Fatalf("expected malformed learnings (%s) to be rejected", name)
			}
		})
	}
}

func containsKey(jsonStr, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
