package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/workflow"
)

func TestContractDocsEnumsMatchWorkflow(t *testing.T) {
	skill := filepath.Join("..", "..", "skills", "gitmoot", "SKILL.md")
	assertDocEnumNear(t, skill, "gitmoot job close", workflow.ResultDecisions)
	assertDocEnumNear(t, skill, "locks, commits, pushes", workflow.DelegationActions)

	contract := filepath.Join("..", "..", "skills", "gitmoot", "references", "RESULT_CONTRACT.md")
	assertDocEnumNear(t, contract, `"decision"`, workflow.ResultDecisions)
	assertDocEnumNear(t, contract, "failure_policy", workflow.DelegationFailurePolicies)
	assertDocEnumNear(t, contract, "synthesis_rule", workflow.DelegationSynthesisRules)
}

func assertDocEnumNear(t *testing.T, path, marker string, values []string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(contents), "\n")
	for i, line := range lines {
		if !strings.Contains(line, marker) {
			continue
		}
		end := min(i+3, len(lines))
		block := strings.Join(lines[i:end], " ")
		for _, value := range values {
			if !containsDocEnumToken(block, value) {
				t.Fatalf("%s enum near %q omits canonical value %q", path, marker, value)
			}
		}
		return
	}
	t.Fatalf("%s has no enum context containing %q", path, marker)
}

func TestContainsDocEnumTokenRejectsSubstring(t *testing.T) {
	if containsDocEnumToken("a queued task", "ask") {
		t.Fatal("ask must not match within task")
	}
	if !containsDocEnumToken("--action ask|review|implement", "ask") {
		t.Fatal("standalone ask was not matched")
	}
}

func containsDocEnumToken(text, value string) bool {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(value) + `\b`).MatchString(text)
}
