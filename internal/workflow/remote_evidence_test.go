package workflow

import "testing"

func TestRemoteChecksEvidenceCeiling(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tests    []string
		evidence string
		clamped  bool
	}{
		{"ran", []string{"go test ./... -> ok"}, EvidenceExecuted, false},
		{"command not found", []string{"go vet ./... -> ok", "swift build -> bash: swift: command not found"}, EvidenceExecuted, true},
		{"toolchain unavailable", []string{"cargo test --locked (Rust toolchain unavailable in sandbox)"}, EvidenceExecuted, true},
		{"exit 127", []string{"pnpm test -> exit 127"}, EvidenceExecuted, true},
		{"nothing ran", []string{"could not run"}, EvidenceExecuted, true},
		{"empty", nil, EvidenceExecuted, true},
		{"already static", []string{"swift: command not found"}, EvidenceStaticOnly, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := AgentResult{Evidence: tc.evidence, TestsRun: tc.tests}
			reason, clamped := ApplyRemoteChecksEvidenceCeiling(&result)
			if clamped != tc.clamped {
				t.Fatalf("clamped = %v (%q); want %v", clamped, reason, tc.clamped)
			}
			if tc.clamped && (result.Evidence != EvidenceStaticOnly || !result.EvidenceDeclared || reason == "") {
				t.Fatalf("clamped result %+v reason %q", result, reason)
			}
			if !tc.clamped && result.Evidence != tc.evidence {
				t.Fatalf("evidence moved to %s", result.Evidence)
			}
		})
	}
}
