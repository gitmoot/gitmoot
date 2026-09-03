package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// The exact envelope shape that failed in production: review job
// local-review-gm-review-opus-18d1b91052453964 reported its tests as objects,
// the strict element decode failed the WHOLE result with "json: cannot
// unmarshal object into Go struct field AgentResult.tests_run of type string",
// and it burned repair_retry attempt 1 of 2. A job that did the work and
// reported it richly was indistinguishable from one that returned nothing.
func TestExtractAgentResultAcceptsObjectElementsInTestsRun(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"approved","summary":"docs change verified","severity":"P3",` +
		`"findings":[],"changes_made":[],` +
		`"tests_run":[{"name":"PR diff scope confirmation","command":"git show --stat 663321c9","result":"pass","detail":"one file changed"},` +
		`"go test ./internal/workflow/"],` +
		`"needs":[],"delegations":[]}}`

	result, err := ExtractAgentResult(output)
	if err != nil {
		t.Fatalf("ExtractAgentResult returned error for object tests_run elements: %v", err)
	}
	if len(result.TestsRun) != 2 {
		t.Fatalf("tests_run = %#v, want both entries preserved", result.TestsRun)
	}
	// The object element keeps every field it carried: choosing one field would
	// be a guess (the contract names none) and would drop what was measured.
	for _, want := range []string{"PR diff scope confirmation", "git show --stat 663321c9", "pass", "one file changed"} {
		if !strings.Contains(result.TestsRun[0], want) {
			t.Fatalf("object element %q lost %q", result.TestsRun[0], want)
		}
	}
	if result.TestsRun[1] != "go test ./internal/workflow/" {
		t.Fatalf("string element = %q, want it unchanged", result.TestsRun[1])
	}
}

// Object elements are re-encoded through a map so key order is canonical: two
// agents reporting the same object must produce the same entry, or a digest or
// a comparison over tests_run sees a spurious change.
func TestResultStringListCanonicalizesObjectKeyOrder(t *testing.T) {
	var first, second ResultStringList
	if err := json.Unmarshal([]byte(`[{"command":"go test","name":"unit","result":"pass"}]`), &first); err != nil {
		t.Fatalf("unmarshal first: %v", err)
	}
	if err := json.Unmarshal([]byte(`[{"result":"pass","name":"unit","command":"go test"}]`), &second); err != nil {
		t.Fatalf("unmarshal second: %v", err)
	}
	if first[0] != second[0] {
		t.Fatalf("key order changed the entry:\n %q\n %q", first[0], second[0])
	}
}

// The marshalled shape must not change: payloads persist, and every consumer
// (PR comments, printStringList, result digests) reads an array of strings.
func TestResultStringListMarshalsAsAnArrayOfStrings(t *testing.T) {
	var list ResultStringList
	if err := json.Unmarshal([]byte(`["go build ./...",{"name":"unit","result":"pass"}]`), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back []string
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("re-decode as []string failed, so the wire shape changed: %v (%s)", err, encoded)
	}
	if len(back) != 2 || back[0] != "go build ./..." {
		t.Fatalf("round trip = %#v", back)
	}
}

// Nothing beyond the measured shapes is accepted. Widening to numbers,
// booleans, nested arrays, or a bare string in place of the array would change
// the contract on a guess: no agent has been observed sending them.
func TestResultStringListRejectsUnevidencedShapes(t *testing.T) {
	for name, payload := range map[string]string{
		"number element":  `[1]`,
		"boolean element": `[true]`,
		"nested array":    `[["go test"]]`,
		"bare string":     `"go test"`,
		"object instead":  `{"tests_run":"go test"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var list ResultStringList
			if err := json.Unmarshal([]byte(payload), &list); err == nil {
				t.Fatalf("%s was accepted as %#v, want a decode error", name, list)
			}
		})
	}
}

// null and an absent list stay nil so normalizeAgentResult keeps owning the
// empty-slice normalization.
func TestResultStringListAcceptsNull(t *testing.T) {
	list := ResultStringList{"stale"}
	if err := json.Unmarshal([]byte(`null`), &list); err != nil {
		t.Fatalf("null must decode: %v", err)
	}
	if list != nil {
		t.Fatalf("null decoded to %#v, want nil", list)
	}
}

// changes_made and needs share the type and take the same input class, so the
// same object shape must not fail them either.
func TestExtractAgentResultAcceptsObjectElementsInEveryStringList(t *testing.T) {
	output := `{"gitmoot_result":{"decision":"implemented","summary":"did the thing",` +
		`"findings":[],"changes_made":[{"file":"a.go","change":"added a guard"}],` +
		`"tests_run":[],"needs":[{"blocked_on":"owner","why":"merge authority"}],"delegations":[]}}`

	result, err := ExtractAgentResult(output)
	if err != nil {
		t.Fatalf("ExtractAgentResult returned error: %v", err)
	}
	if len(result.ChangesMade) != 1 || !strings.Contains(result.ChangesMade[0], "added a guard") {
		t.Fatalf("changes_made = %#v", result.ChangesMade)
	}
	if len(result.Needs) != 1 || !strings.Contains(result.Needs[0], "merge authority") {
		t.Fatalf("needs = %#v", result.Needs)
	}
}
