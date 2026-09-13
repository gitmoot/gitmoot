package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReviewRouterPools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[review_router]\ncode = [\"devin/swe-2\", \"openai-codex/gpt-5.6-sol\"]\nsecurity = [\"anthropic/claude-opus-4-6\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadReviewRouterSettings(Paths{ConfigFile: path})
	if err != nil {
		t.Fatal(err)
	}
	security, err := settings.Models("security")
	if err != nil || !reflect.DeepEqual(security, []string{"anthropic/claude-opus-4-6"}) {
		t.Fatalf("security pool = %v, %v", security, err)
	}
	ui, err := settings.Models("ui")
	if err != nil || !reflect.DeepEqual(ui, []string{"devin/swe-2", "openai-codex/gpt-5.6-sol"}) {
		t.Fatalf("unspecialized pool = %v, %v", ui, err)
	}
	if _, err := settings.Models("securtiy"); err == nil {
		t.Fatal("unknown purpose silently fell back")
	}
}

func TestReviewRouterRejectsInvalidPools(t *testing.T) {
	for _, entry := range []string{
		`code = []`,
		`code = ["swe-2"]`,
		`code = ["devin/swe-2", "devin/swe-2"]`,
		`security = ["/missing-provider"]`,
		`securtiy = ["devin/swe-2"]`,
	} {
		t.Run(entry, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte("[review_router]\n"+entry+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadReviewRouterSettings(Paths{ConfigFile: path}); err == nil {
				t.Fatal("invalid route admitted")
			}
		})
	}
}

// TestReviewRouterMalformedHeaderEndsSection pins this loader's sectionHeader
// call site. A line that opens a bracket and never closes it is a BOUNDARY that
// names no section, so the keys after it belong to nobody. Reverting the site to
// the pre-#1759 two-bracket form makes `[oops` fall through to the key parser,
// which either refuses the whole config or lands `security` in [review_router] —
// both fail here.
func TestReviewRouterMalformedHeaderEndsSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[review_router]\ncode = [\"devin/swe-2\"]\n[oops\nsecurity = [\"anthropic/claude-opus-4-6\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := LoadReviewRouterSettings(Paths{ConfigFile: path})
	if err != nil {
		t.Fatalf("a malformed header must end the section, not fail the load: %v", err)
	}
	code, err := settings.Models("code")
	if err != nil || !reflect.DeepEqual(code, []string{"devin/swe-2"}) {
		t.Fatalf("code pool = %v, %v; keys BEFORE the malformed header must still apply", code, err)
	}
	security, err := settings.Models("security")
	if err != nil || !reflect.DeepEqual(security, code) {
		t.Fatalf("security pool = %v, %v; a key after the malformed header was attributed to [review_router]", security, err)
	}
}
