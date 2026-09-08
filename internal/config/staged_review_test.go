package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeStagedReviewConfig(t *testing.T, body string) Paths {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Paths{ConfigFile: file}
}

// #1821. The owner's ruling is that stage 2's reviewer is DECLARED per repo and
// an undeclared repo REFUSES rather than picking something. So the only
// interesting behaviours here are: a declaration is read, and the absence of one
// is distinguishable from every other state.

func TestStagedReviewReadsADeclaration(t *testing.T) {
	paths := writeStagedReviewConfig(t, `
[repos."owner/repo"]
max_parallel = 2
staged_review_verdict_agent = "gm-review-opus"
`)
	agent, declared, err := StagedReviewVerdictAgent(paths, "owner/repo")
	if err != nil {
		t.Fatalf("StagedReviewVerdictAgent: %v", err)
	}
	if !declared {
		t.Fatal("a declared verdict agent reported declared=false; the dispatch would refuse a repo that configured one")
	}
	if agent != "gm-review-opus" {
		t.Fatalf("verdict agent = %q, want gm-review-opus", agent)
	}
}

// OFF BY DEFAULT is the property that makes this safe to land while nothing
// dispatches two stages: no declaration anywhere means no behaviour change.
func TestStagedReviewIsOffByDefault(t *testing.T) {
	paths := writeStagedReviewConfig(t, "[daemon]\nworkers = 4\n")
	entries, err := LoadStagedReview(paths)
	if err != nil {
		t.Fatalf("LoadStagedReview: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a config with no declaration produced %d entries", len(entries))
	}
	if _, declared, err := StagedReviewVerdictAgent(paths, "owner/repo"); err != nil || declared {
		t.Fatalf("an undeclared repo reported declared=%v err=%v; want false and no error", declared, err)
	}
}

// A repo with a [repos.*] section but NO staged-review key is undeclared. This
// is the distinction the refusal rests on: having a section is not declaring a
// reviewer, and confusing the two would make every repo with a max_parallel
// override look like it had opted into staged review.
func TestStagedReviewSectionWithoutTheKeyIsUndeclared(t *testing.T) {
	paths := writeStagedReviewConfig(t, "[repos.\"owner/repo\"]\nmax_parallel = 2\n")
	agent, declared, err := StagedReviewVerdictAgent(paths, "owner/repo")
	if err != nil {
		t.Fatalf("StagedReviewVerdictAgent: %v", err)
	}
	if declared || agent != "" {
		t.Fatalf("a section with no staged-review key reported declared=%v agent=%q", declared, agent)
	}
}

// An empty declaration is dropped rather than surfaced as a declared-but-unnamed
// agent, so no caller can dispatch to "".
func TestStagedReviewEmptyDeclarationIsNotADeclaration(t *testing.T) {
	paths := writeStagedReviewConfig(t, "[repos.\"owner/repo\"]\nstaged_review_verdict_agent = \"\"\n")
	agent, declared, err := StagedReviewVerdictAgent(paths, "owner/repo")
	if err != nil {
		t.Fatalf("StagedReviewVerdictAgent: %v", err)
	}
	if declared || agent != "" {
		t.Fatalf("an empty declaration reported declared=%v agent=%q", declared, agent)
	}
}

func TestStagedReviewMatchesRepoCaseInsensitively(t *testing.T) {
	paths := writeStagedReviewConfig(t, "[repos.\"Gitmoot/Gitmoot\"]\nstaged_review_verdict_agent = \"g7-review\"\n")
	agent, declared, err := StagedReviewVerdictAgent(paths, "gitmoot/gitmoot")
	if err != nil {
		t.Fatalf("StagedReviewVerdictAgent: %v", err)
	}
	if !declared || agent != "g7-review" {
		t.Fatalf("case-differing repo did not match: declared=%v agent=%q", declared, agent)
	}
}

// TestStagedReviewMalformedHeaderClearsSection is the PIN for this file's
// sectionHeader call site (#1759's registry).
//
// A line that opens with '[' and never closes is a boundary that names NO
// section, so the current section is cleared and keys under it are dropped.
// Reverting the site to the pre-#1759 two-bracket form makes the malformed line
// not a header at all, leaving the previous section open - and this config is
// arranged so that misattribution is OBSERVABLE: the stray key would overwrite
// a declaration that a reader can check.
func TestStagedReviewMalformedHeaderClearsSection(t *testing.T) {
	paths := writeStagedReviewConfig(t, `
[repos."owner/repo"]
staged_review_verdict_agent = "declared-reviewer"

[repos."other/repo"
staged_review_verdict_agent = "smuggled-reviewer"
`)
	agent, declared, err := StagedReviewVerdictAgent(paths, "owner/repo")
	if err != nil {
		t.Fatalf("StagedReviewVerdictAgent: %v", err)
	}
	if !declared {
		t.Fatal("the valid declaration was lost")
	}
	if agent != "declared-reviewer" {
		t.Fatalf("verdict agent = %q; a key under a MALFORMED header was misattributed to the previous repo, "+
			"which is the pre-#1759 two-bracket behaviour", agent)
	}
}
