package workflow

import (
	"strings"
	"testing"
)

// TestExtractAgentResultRefusesRetiredFieldsByName pins #2588: the delegation
// engine and the human-question pause are gone, so a result that still carries
// `delegations` or `human_questions` (even empty) is a named contract failure for
// every decision, not a silently dropped request.
func TestExtractAgentResultRefusesRetiredFieldsByName(t *testing.T) {
	for _, field := range []string{"delegations", "human_questions"} {
		for _, decision := range []string{"approved", "changes_requested", "implemented", "failed", "skipped"} {
			output := `{"gitmoot_result":{"decision":"` + decision + `","summary":"s","findings":[],"changes_made":[],"tests_run":[],"needs":[],"` + field + `":[]}}`
			_, err := ExtractAgentResult(output)
			if err == nil {
				t.Fatalf("%s/%s: ExtractAgentResult accepted a retired field", field, decision)
			}
			msg := err.Error()
			if !strings.Contains(msg, `unsupported gitmoot_result field "`+field+`"`) {
				t.Fatalf("%s/%s: error does not name the field: %v", field, decision, err)
			}
			if !strings.Contains(msg, "no longer") {
				t.Fatalf("%s/%s: error does not explain the retirement: %v", field, decision, err)
			}
		}
	}
}
