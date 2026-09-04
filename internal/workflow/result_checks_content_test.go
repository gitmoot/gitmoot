package workflow

import "testing"

// TestEntryCarriesContentRejectsContainersWithOnlyEmptyValues is the round-N F3:
// the false-accept was only HALF closed. Counting keys made
// {"name":"","command":""} read as populated evidence, which is worse than a
// hard failure because a coordinator sees a populated tests_run and believes
// the job looked.
func TestEntryCarriesContentRejectsContainersWithOnlyEmptyValues(t *testing.T) {
	empty := map[string]string{
		"empty object":              `{}`,
		"object of empty strings":   `{"name":"","command":""}`,
		"nested empty object":       `{"a":{"b":""}}`,
		"empty array":               `[]`,
		"array of empty strings":    `["",""]`,
		"array of empty containers": `[{},[]]`,
		"whitespace":                "   ",
		"empty string":              "",
	}
	for name, value := range empty {
		t.Run("no content/"+name, func(t *testing.T) {
			if entryCarriesContent(value) {
				t.Fatalf("%s reads as evidence but carries no information", name)
			}
		})
	}

	populated := map[string]string{
		"object with a value":       `{"name":"diff scope"}`,
		"object with one populated": `{"name":"","command":"go test ./..."}`,
		"nested populated":          `{"a":{"b":"real"}}`,
		"array with a value":        `["go test ./..."]`,
		"plain text":                "go test ./...",
		"unparseable container":     `{not json`,
	}
	for name, value := range populated {
		t.Run("content/"+name, func(t *testing.T) {
			if !entryCarriesContent(value) {
				t.Fatalf("%s carries information but was rejected", name)
			}
		})
	}
}

// TestImplementGatesAgreeWithTheNeedsGate is the round-N F5: implement-changes-listed
// and implement-tests-listed used raw len(), so the SAME {} the needs gate
// rejects satisfied them. Two gates disagreeing about one input is the defect.
func TestImplementGatesAgreeWithTheNeedsGate(t *testing.T) {
	contentFree := []string{"{}"}
	if hasActionableEntries(contentFree) {
		t.Fatal("the needs gate would reject {} - the shared test must too")
	}

	// Driven through the production gate list rather than the helper: an
	// implement result whose only entries are content-free must not pass the
	// listed checks.
	result := AgentResult{
		Decision:    "implemented",
		Summary:     "did the thing",
		ChangesMade: []string{"{}"},
		TestsRun:    []string{"{}"},
	}
	checks := RunResultChecks(ResultCheckInput{Action: "implement", Result: result})
	for _, check := range checks {
		switch check.ID {
		case "implement-changes-listed", "implement-tests-listed":
			if check.Pass {
				t.Errorf("%s passed on a content-free entry that the needs gate rejects", check.ID)
			}
		}
	}

	// A real entry must still pass, so this is not simply refusing everything.
	real := AgentResult{
		Decision:    "implemented",
		Summary:     "did the thing",
		ChangesMade: []string{"internal/workflow/result.go: fixed the gate"},
		TestsRun:    []string{"go test ./internal/workflow/"},
	}
	for _, check := range RunResultChecks(ResultCheckInput{Action: "implement", Result: real}) {
		switch check.ID {
		case "implement-changes-listed", "implement-tests-listed":
			if !check.Pass {
				t.Errorf("%s failed on a populated entry", check.ID)
			}
		}
	}
}
