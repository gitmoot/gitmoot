package cli

import (
	"encoding/json"
	"testing"
)

// Test helpers shared with UNTAGGED tests, kept out of the e2e build tag
// (#1760 step 3). Tagging the e2e file beside this one removes it from the
// default build; these symbols are used by tests that are not e2e, so they
// would have stopped compiling. Types are moved WITH their methods.

func pipelineShellResultCommand(t *testing.T, decision, summary string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"gitmoot_result": map[string]any{
			"decision": decision, "summary": summary,
			"findings": []any{}, "changes_made": []any{}, "tests_run": []any{},
			"needs": []any{}, "delegations": []any{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return `printf '%s' ` + posixQuote(string(raw))
}
