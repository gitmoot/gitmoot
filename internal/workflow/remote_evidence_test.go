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
		// #2520: a CI-verified platform check names its local absence on
		// purpose; with real local runs beside it the review stays executed.
		{"local runs plus CI-verified check", []string{"bash -n Tools/ci/ui-shard.sh -> ok", "CI-verified: xcodebuild test -> success; run 41841861442 job 126845298023; exact head verified; Xcode toolchain absent locally"}, EvidenceExecuted, false},
		// Only CI-verified entries: the review itself ran nothing.
		{"only CI-verified", []string{"CI-verified: xcodebuild test -> success; run 1 job 2; Xcode toolchain absent locally"}, EvidenceExecuted, true},
		// The exemption is the prefix, not the word: a local entry still clamps.
		{"local missing toolchain mentioning CI", []string{"go vet ./... -> ok", "swift build (CI-verified later) -> swift: command not found"}, EvidenceExecuted, true},
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
