package config

import (
	"os"
	"path/filepath"
	"testing"
)

// #1969. A repository declares whether it consumes review findings. The parse
// rules matter more than usual here because this is the one review-policy field
// that can RELAX a merge gate, so every ambiguous input must land on consuming.
func TestFindingsConsumptionParsesAndFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		wantAdvisory bool
		wantDeclared string
		wantErr      bool
	}{
		{
			name:         "unset is undeclared and still consuming",
			body:         "[review]\nblocking_severity = \"P2\"\n",
			wantDeclared: "",
		},
		{
			name:         "an explicit consuming declaration is recorded and still consuming",
			body:         "[review]\nfindings_consumption = \"consuming\"\n",
			wantDeclared: FindingsConsuming,
		},
		{
			name:         "advisory is the only value that relaxes",
			body:         "[review]\nfindings_consumption = \"advisory\"\n",
			wantAdvisory: true,
			wantDeclared: FindingsAdvisory,
		},
		{
			name:         "case is not a way to smuggle a different value",
			body:         "[review]\nfindings_consumption = \"ADVISORY\"\n",
			wantAdvisory: true,
			wantDeclared: FindingsAdvisory,
		},
		{
			// A typo must not read as undeclared-and-therefore-fine: the operator
			// plainly meant to relax the gate and will believe they did, so the
			// value is refused AND the policy stays consuming.
			name:         "a misspelled declaration errors and stays consuming",
			body:         "[review]\nfindings_consumption = \"advisroy\"\n",
			wantDeclared: "",
			wantErr:      true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			paths := PathsForHome(home)
			if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.ConfigFile, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadReviewConfig(paths)
			if tc.wantErr && err == nil {
				t.Fatal("an unsupported findings_consumption was accepted silently")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("LoadReviewConfig: %v", err)
			}
			policy := cfg.For("owner/repo")
			if policy.FindingsConsumption != tc.wantDeclared {
				t.Errorf("declaration = %q, want %q", policy.FindingsConsumption, tc.wantDeclared)
			}
			if policy.FindingsAreAdvisory() != tc.wantAdvisory {
				t.Errorf("advisory = %v, want %v; only an explicit advisory may relax the obligation gate",
					policy.FindingsAreAdvisory(), tc.wantAdvisory)
			}
		})
	}
}

// A per-repository declaration wins over the global one, and it wins in BOTH
// directions: a fleet that has declared itself advisory must still be able to
// hold one repository to consuming, or the knob can only ever loosen.
func TestFindingsConsumptionRepositoryOverrideWinsBothWays(t *testing.T) {
	home := t.TempDir()
	paths := PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[review]\nfindings_consumption = \"advisory\"\n\n" +
		"[repos.\"owner/strict\".review]\nfindings_consumption = \"consuming\"\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadReviewConfig(paths)
	if err != nil {
		t.Fatalf("LoadReviewConfig: %v", err)
	}
	if !cfg.For("owner/loose").FindingsAreAdvisory() {
		t.Error("the global advisory declaration did not reach an unlisted repository")
	}
	if cfg.For("owner/strict").FindingsAreAdvisory() {
		t.Error("a repository declared consuming was relaxed by the global advisory declaration")
	}
}
