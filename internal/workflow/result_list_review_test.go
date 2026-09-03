package workflow

import (
	"strings"
	"testing"
)

// envelopeWithList builds a minimal valid gitmoot_result envelope whose named
// list field carries the given JSON array, so every case below enters through
// the PRODUCTION path (ExtractAgentResult) rather than poking the decoder.
func envelopeWithList(field, list string) string {
	lists := map[string]string{"changes_made": "[]", "tests_run": "[]", "needs": "[]"}
	lists[field] = list
	return `{"gitmoot_result":{"decision":"approved","summary":"s","findings":[],"changes_made":` +
		lists["changes_made"] + `,"tests_run":` + lists["tests_run"] +
		`,"needs":` + lists["needs"] + `,"delegations":[]}}`
}

// TestNullListElementStaysAnEmptyEntry is the #1809 P2 regression.
//
// A JSON null element PARSED at parent 0965c458 and decoded to an empty entry:
// ["a",null,"b"] produced ["a","","b"], verified differentially against that
// commit. The first version of this PR rejected it, which failed the whole
// envelope and burned a repair attempt - the exact failure #1805 exists to
// remove, arriving as a narrowing alongside the intended widening.
//
// It fails if a null element ever starts failing the envelope again.
func TestNullListElementStaysAnEmptyEntry(t *testing.T) {
	for _, field := range []string{"changes_made", "tests_run", "needs"} {
		result, err := ExtractAgentResult(envelopeWithList(field, `["a",null,"b"]`))
		if err != nil {
			t.Fatalf("%s: a null element must not fail the envelope: %v", field, err)
		}
		var got ResultStringList
		switch field {
		case "changes_made":
			got = result.ChangesMade
		case "tests_run":
			got = result.TestsRun
		case "needs":
			got = result.Needs
		}
		if len(got) != 3 || got[0] != "a" || got[1] != "" || got[2] != "b" {
			t.Fatalf("%s = %#v, want [a, \"\", b] to match parent 0965c458", field, got)
		}
	}
}

// TestObjectElementKeepsShellMetacharacters is the P3-1 regression. json.Marshal
// HTML-escapes <, > and &, so a recorded command came back as
// `go test ./... 2\u003e\u00261` - unusable as evidence, because a tests_run
// entry that cannot be copy-pasted back is not the command that ran.
func TestObjectElementKeepsShellMetacharacters(t *testing.T) {
	// The command is embedded LITERALLY, as a runtime actually emits it: raw <, >
	// and & are legal inside a JSON string. Building this input with json.Marshal
	// would escape them BEFORE the decoder saw them - json.Marshal HTML-escapes by
	// default - and json.RawMessage copies values verbatim, so the escapes would
	// survive and the test would fail for its own input rather than for the
	// behaviour under test. That is exactly how the first version of this test
	// failed.
	const command = `go test ./... 2>&1 && echo <done>`
	result, err := ExtractAgentResult(envelopeWithList("tests_run", `[{"command":"`+command+`"}]`))
	if err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if len(result.TestsRun) != 1 {
		t.Fatalf("tests_run = %#v, want one entry", result.TestsRun)
	}
	if !strings.Contains(result.TestsRun[0], command) {
		t.Fatalf("entry %q lost the verbatim command %q", result.TestsRun[0], command)
	}
	for _, escaped := range []string{`\u003e`, `\u0026`, `\u003c`} {
		if strings.Contains(result.TestsRun[0], escaped) {
			t.Fatalf("entry %q contains HTML escape %s", result.TestsRun[0], escaped)
		}
	}
}

// TestDuplicateKeysAreRejectedNotCollapsed is the P3-3 regression.
// map[string]json.RawMessage silently keeps the LAST duplicate, so
// [{"name":"first","name":"second"}] decoded without error to {"name":"second"},
// losing a field while the docs claimed every field is kept. Duplicate keys are
// exactly the malformed-but-parseable JSON a runtime emits; silence is the one
// option that is wrong.
func TestDuplicateKeysAreRejectedNotCollapsed(t *testing.T) {
	_, err := ExtractAgentResult(envelopeWithList("tests_run", `[{"name":"first","name":"second"}]`))
	if err == nil {
		t.Fatal("duplicate keys were accepted; they must be rejected rather than silently collapsed")
	}
	if !strings.Contains(err.Error(), "duplicate key") || !strings.Contains(err.Error(), "name") {
		t.Fatalf("error %q must name the duplicate key", err)
	}
}

// TestListElementErrorNamesItsField is the P3-4 regression. Pre-#1805
// encoding/json reported "AgentResult.tests_run"; the custom decoder's error is
// passed through unwrapped, so the name was lost - and a repair attempt that
// cannot see WHICH list failed is a wasted attempt.
func TestListElementErrorNamesItsField(t *testing.T) {
	for _, field := range []string{"changes_made", "tests_run", "needs"} {
		_, err := ExtractAgentResult(envelopeWithList(field, `[123]`))
		if err == nil {
			t.Fatalf("%s: a number element must be rejected", field)
		}
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("error for %s does not name the field: %v", field, err)
		}
	}
}

// TestReachableDecoderGuardsArePinned covers the two guards that survive the
// P3-5 cleanup. Both are reachable ONLY by a direct call, which is legitimate
// because UnmarshalJSON is exported: the element guards were deleted instead,
// because encoding/json never produces a padded or zero-length element and no
// mutant could kill them.
func TestReachableDecoderGuardsArePinned(t *testing.T) {
	var nilList *ResultStringList
	if err := nilList.UnmarshalJSON([]byte(`["a"]`)); err == nil {
		t.Fatal("nil receiver must error rather than panic")
	}
	var list ResultStringList
	if err := list.UnmarshalJSON(nil); err != nil {
		t.Fatalf("empty data must decode to nil, got %v", err)
	}
	if list != nil {
		t.Fatalf("empty data produced %#v, want nil", list)
	}
}

// TestEmptyObjectEntryIsNotActionableEvidence is the P3-6 regression, and it is
// a GATE behaviour test rather than a decoder one. needs:[{}] now decodes to the
// entry "{}", which is non-empty to TrimSpace, so a BLOCKED result whose only
// blocker was an empty object passed the evidence heuristic.
func TestEmptyObjectEntryIsNotActionableEvidence(t *testing.T) {
	for _, empty := range []string{"{}", "[]", "", "   "} {
		if hasActionableEntries([]string{empty}) {
			t.Fatalf("entry %q must not count as actionable", empty)
		}
	}
	for _, real := range []string{`{"blocker":"needs credentials"}`, "run the migration", "{unparseable"} {
		if !hasActionableEntries([]string{real}) {
			t.Fatalf("entry %q must count as actionable", real)
		}
	}
}
