package config

import (
	"path/filepath"
	"strings"
	"testing"
)

const fixtureCleanupBudget = `limit_bytes = 1048576
owner = "fixture-owner"
approval = "fixture-only-decision"
max_entries = 100000
max_depth = 64
timeout_ms = 30000
`

// #2360: a budget exists only where one section supplies all six keys; a repository section
// replaces the global one; no section means nil (UNKNOWN), never zero or unlimited.
func TestLoadCleanupBudgetResolvesScopesWithoutMixing(t *testing.T) {
	cfg, err := LoadCleanupBudget(writeGateConfig(t, "[cleanup_budget]\n"+fixtureCleanupBudget+
		"[repos.\"o/special\".cleanup_budget]\n"+strings.Replace(fixtureCleanupBudget, "1048576", "4096", 1)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	global := cfg.For("o/other")
	if global == nil || global.LimitBytes != 1048576 || global.Owner != "fixture-owner" || global.Approval != "fixture-only-decision" ||
		global.MaxEntries != 100000 || global.MaxDepth != 64 || global.TimeoutMS != 30000 {
		t.Fatalf("global budget = %+v", global)
	}
	if special := cfg.For("o/special"); special == nil || special.LimitBytes != 4096 {
		t.Fatalf("repo budget = %+v, want the repo section (4096)", special)
	}

	none, err := LoadCleanupBudget(writeGateConfig(t, "[workflow]\nrequire_workflow = true\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if b := none.For("o/r"); b != nil {
		t.Fatalf("budget without any section = %+v, want nil (unknown)", b)
	}
	missing, err := LoadCleanupBudget(Paths{ConfigFile: filepath.Join(t.TempDir(), "absent.toml")})
	if err != nil || missing.For("o/r") != nil {
		t.Fatalf("missing config file: budget=%+v err=%v, want nil, nil", missing.For("o/r"), err)
	}
}

// #2360: an incomplete, invalid or ambiguous budget is a load error (the caller refuses), never a
// budget assembled from defaults or from another scope.
func TestLoadCleanupBudgetRefusesIncompleteOrInvalidSections(t *testing.T) {
	cases := map[string]string{
		"missing owner":     "[cleanup_budget]\n" + strings.Replace(fixtureCleanupBudget, "owner = \"fixture-owner\"\n", "", 1),
		"missing limits":    "[cleanup_budget]\nlimit_bytes = 1\nowner = \"o\"\napproval = \"a\"\n",
		"zero cap":          "[cleanup_budget]\n" + strings.Replace(fixtureCleanupBudget, "1048576", "0", 1),
		"negative cap":      "[cleanup_budget]\n" + strings.Replace(fixtureCleanupBudget, "1048576", "-5", 1),
		"overflowing cap":   "[cleanup_budget]\n" + strings.Replace(fixtureCleanupBudget, "1048576", "99999999999999999999", 1),
		"unquoted owner":    "[cleanup_budget]\n" + strings.Replace(fixtureCleanupBudget, "\"fixture-owner\"", "fixture-owner", 1),
		"unknown key":       "[cleanup_budget]\n" + fixtureCleanupBudget + "unlimited = true\n",
		"duplicate key":     "[cleanup_budget]\n" + fixtureCleanupBudget + "limit_bytes = 5\n",
		"duplicate section": "[cleanup_budget]\n" + fixtureCleanupBudget + "[cleanup_budget]\n" + fixtureCleanupBudget,
		"huge depth":        "[cleanup_budget]\n" + strings.Replace(fixtureCleanupBudget, "max_depth = 64", "max_depth = 100000", 1),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCleanupBudget(writeGateConfig(t, content)); err == nil {
				t.Fatalf("load succeeded for %s; a cap must not be assembled from an invalid section", name)
			}
		})
	}
}

// #2360: a malformed header naming cleanup_budget refuses the whole file, so keys after it can
// never be read as a cap; a typo in an unrelated section does not.
func TestCleanupBudgetMalformedHeaderRefuses(t *testing.T) {
	if _, err := LoadCleanupBudget(writeGateConfig(t, "[cleanup_budget\n"+fixtureCleanupBudget)); err == nil {
		t.Fatal("a malformed [cleanup_budget header was accepted")
	}
	cfg, err := LoadCleanupBudget(writeGateConfig(t, "[cleanup_budget]\n"+fixtureCleanupBudget+"[disk_guard\nmin_free_bytes = 1\n"))
	if err != nil || cfg.For("o/r") == nil {
		t.Fatalf("an unrelated malformed header broke the budget: cfg=%+v err=%v", cfg.For("o/r"), err)
	}
}
