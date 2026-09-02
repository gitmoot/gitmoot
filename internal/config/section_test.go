package config

import (
	"os"
	"strings"
	"testing"
)

// TestSectionHeaderClassification is the helper table. The load-bearing rows are
// the malformed ones: a line that opens a bracket and never closes it must be
// reported as a BOUNDARY that names no section, which is what clears the
// caller's state instead of misattributing the keys that follow.
func TestSectionHeaderClassification(t *testing.T) {
	for _, tc := range []struct {
		name     string
		line     string
		wantName string
		wantOK   bool
	}{
		{name: "valid", line: "[workflow]", wantName: "workflow", wantOK: true},
		{name: "valid with inner spaces", line: "[ workflow ]", wantName: "workflow", wantOK: true},
		{name: "valid dotted", line: "[agents.planner]", wantName: "agents.planner", wantOK: true},
		{name: "valid empty name", line: "[]", wantName: "", wantOK: true},

		// The correction. Each of these previously failed the two-bracket test
		// outright, so it was not treated as a header at all and the PREVIOUS
		// section stayed open.
		{name: "malformed unclosed", line: "[workflow", wantName: "", wantOK: true},
		{name: "malformed unclosed dotted", line: "[agents.planner", wantName: "", wantOK: true},
		{name: "malformed bare bracket", line: "[", wantName: "", wantOK: true},

		// Not headers at all: no leading bracket, so no boundary.
		{name: "key", line: "enabled = true", wantName: "", wantOK: false},
		{name: "value with brackets", line: "roots = [\"a\"]", wantName: "", wantOK: false},
		{name: "empty", line: "", wantName: "", wantOK: false},
		{name: "trailing bracket only", line: "workflow]", wantName: "", wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotOK := sectionHeader(tc.line)
			if gotName != tc.wantName || gotOK != tc.wantOK {
				t.Fatalf("sectionHeader(%q) = (%q, %v), want (%q, %v)", tc.line, gotName, gotOK, tc.wantName, tc.wantOK)
			}
		})
	}
}

// TestMalformedHeaderNeverMisattributesKeys is the regression that matters, and
// it runs through PRODUCTION LOADERS rather than the helper: a table test on
// sectionHeader would stay green if a loader kept its own inline classification,
// which is exactly the mutant this consolidation could leave behind.
//
// The fixture is the hazard shape from the #1113 finder: a valid section, then a
// botched header with no closing bracket, then a key that the malformed header
// must prevent from landing in the earlier section.
func TestMalformedHeaderNeverMisattributesKeys(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[admission]
max_memory_gb = 1.0

[admission
max_memory_gb = 99.0
`), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := LoadAdmissionPolicy(paths)
	if err != nil {
		t.Fatalf("LoadAdmissionPolicy: %v", err)
	}
	if policy.MaxMemoryGB == 99.0 {
		t.Fatal("a key after a malformed header was applied to [admission]; the header must end the section")
	}
	if policy.MaxMemoryGB != 1.0 {
		t.Fatalf("MaxMemoryGB = %v, want 1.0 (keys BEFORE the malformed header must still apply)", policy.MaxMemoryGB)
	}
}

// TestValidHeadersRemainByteEquivalent pins the other half of the contract: only
// the invalid-input path changed. A consolidation that quietly altered trimming
// or name resolution would show up here rather than in production.
func TestValidHeadersRemainByteEquivalent(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	// Odd but VALID spacing, plus an unrelated section in between, so section
	// switching and trimming are both exercised.
	if err := os.WriteFile(paths.ConfigFile, []byte(`
[ admission ]
max_memory_gb = 2.5
[workflow]
result_checks = block
[admission]
max_concurrent_sessions = 7
`), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := LoadAdmissionPolicy(paths)
	if err != nil {
		t.Fatalf("LoadAdmissionPolicy: %v", err)
	}
	if policy.MaxMemoryGB != 2.5 {
		t.Fatalf("MaxMemoryGB = %v, want 2.5 from the spaced-but-valid header", policy.MaxMemoryGB)
	}
	if policy.MaxConcurrentSessions != 7 {
		t.Fatalf("MaxConcurrentSessions = %d, want 7 from the re-opened [admission] section", policy.MaxConcurrentSessions)
	}
	mode, err := LoadResultChecksMode(paths)
	if err != nil {
		t.Fatalf("LoadResultChecksMode: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(mode)), "block") {
		t.Fatalf("result_checks mode = %q, want block (the interleaved section must still parse)", mode)
	}
}
