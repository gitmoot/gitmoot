package workflow

import "testing"

func TestRemoteChecksEvidenceCeiling(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tests    []string
		evidence string
		clamped  bool
		changes  []string
	}{
		{"ran", []string{"go test ./... -> ok"}, EvidenceExecuted, false, nil},
		{"command not found", []string{"go vet ./... -> ok", "swift build -> bash: swift: command not found"}, EvidenceExecuted, true, nil},
		{"toolchain unavailable", []string{"cargo test --locked (Rust toolchain unavailable in sandbox)"}, EvidenceExecuted, true, nil},
		{"exit 127", []string{"pnpm test -> exit 127"}, EvidenceExecuted, true, nil},
		{"nothing ran", []string{"could not run"}, EvidenceExecuted, true, nil},
		{"empty", nil, EvidenceExecuted, true, nil},
		{"already static", []string{"swift: command not found"}, EvidenceStaticOnly, false, nil},
		// #2520: a CI-verified platform check names its local absence on
		// purpose; with real local runs beside it the review stays executed.
		{"local runs plus CI-verified check", []string{"bash -n Tools/ci/ui-shard.sh -> ok", "CI-verified: xcodebuild test -> success; run 41841861442 job 126845298023; exact head verified; Xcode toolchain absent locally"}, EvidenceExecuted, false, nil},
		// Only CI-verified entries: the review itself ran nothing.
		{"only CI-verified", []string{"CI-verified: xcodebuild test -> success; run 1 job 2; Xcode toolchain absent locally"}, EvidenceExecuted, true, nil},
		// #2522 review: the prefix must not hide a local lookup failure, and
		// an entry without a successful run and job is not CI-verified.
		{"prefixed local lookup failure", []string{"go vet ./... -> ok", "CI-verified: swift build -> bash: swift: command not found"}, EvidenceExecuted, true, nil},
		{"prefixed without run and job", []string{"go vet ./... -> ok", "CI-verified: xcodebuild test -> success; Xcode toolchain absent locally"}, EvidenceExecuted, true, nil},
		// #2522 review: with CI-verified entries, changes_made prose cannot
		// stand in for a local run.
		{"only CI-verified plus changes_made prose", []string{"CI-verified: xcodebuild test -> success; run 1 job 2"}, EvidenceExecuted, true, []string{"No changes made; reviewed internal/workflow/remote_evidence.go"}},
		// The exemption is the prefix, not the word: a local entry still clamps.
		{"local missing toolchain mentioning CI", []string{"go vet ./... -> ok", "swift build (CI-verified later) -> swift: command not found"}, EvidenceExecuted, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := AgentResult{Evidence: tc.evidence, TestsRun: tc.tests, ChangesMade: tc.changes}
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
