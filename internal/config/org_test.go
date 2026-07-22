package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadOrgAndScopeMatching(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "[org]\nenforce = \"block\"\n[org.roles.\"owner\"]\nscope = [\"*\"]\n[org.roles.\"maintainer\"]\nparent = \"owner\"\nscope = [\"Acme/*\", \"other/repo\"]\nmerge_rule = \"self\"\npane = \"w1:p2\"\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled() || cfg.Enforce() != "block" || len(cfg.Roots()) != 1 || cfg.Roots()[0] != "owner" {
		t.Fatalf("cfg=%+v roots=%v", cfg, cfg.Roots())
	}
	role, ok := cfg.Role("maintainer")
	if !ok || role.Scope[0] != "acme/*" || role.Pane != "w1:p2" {
		t.Fatalf("role=%+v ok=%v", role, ok)
	}
	if !ScopeMatches(role.Scope, "ACME/one") || !ScopeMatches([]string{"*"}, "any/repo") || ScopeMatches(role.Scope, "acme") || ScopeMatches(role.Scope, "wrong/repo") {
		t.Fatal("scope matching mismatch")
	}
}

func TestOrgConfigAncestors(t *testing.T) {
	cfg := OrgConfig{roles: map[string]OrgRole{
		"owner":      {Name: "owner"},
		"maintainer": {Name: "maintainer", Parent: "owner"},
		"operator":   {Name: "operator", Parent: "maintainer"},
	}}
	if got, want := cfg.Ancestors("operator"), []string{"maintainer", "owner"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Ancestors(operator) = %v, want %v", got, want)
	}
	if got := cfg.Ancestors("missing"); len(got) != 0 {
		t.Fatalf("Ancestors(missing) = %v, want none", got)
	}
	cycle := OrgConfig{roles: map[string]OrgRole{
		"one": {Name: "one", Parent: "two"},
		"two": {Name: "two", Parent: "one"},
	}}
	if got, want := cycle.Ancestors("one"), []string{"two", "one"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cycle-safe Ancestors(one) = %v, want %v", got, want)
	}
}

func TestOrgConfigChildrenAndPath(t *testing.T) {
	cfg := OrgConfig{roles: map[string]OrgRole{
		"owner":    {Name: "owner", Scope: []string{"*"}},
		"platform": {Name: "platform", Parent: "owner", Scope: []string{"gitmoot/*"}},
		"docs":     {Name: "docs", Parent: "owner", Scope: []string{"gitmoot/docs"}},
		"api":      {Name: "api", Parent: "platform", Scope: []string{"gitmoot/api"}},
	}}
	if got, want := cfg.Children("owner"), []OrgRole{cfg.roles["docs"], cfg.roles["platform"]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Children(owner) = %+v, want %+v", got, want)
	}
	if got, want := cfg.Path("api"), []string{"owner", "platform", "api"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Path(api) = %v, want %v", got, want)
	}
	if got := cfg.Path("missing"); got != nil {
		t.Fatalf("Path(missing) = %v, want nil", got)
	}
	cycle := OrgConfig{roles: map[string]OrgRole{
		"a": {Name: "a", Parent: "b"},
		"b": {Name: "b", Parent: "a"},
	}}
	if got := cycle.Path("a"); len(got) == 0 || len(got) > len(cycle.roles) {
		t.Fatalf("cycle-safe Path(a) = %v, want a bounded non-empty path", got)
	}
}

func TestLoadOrgToleratesUnrelatedMalformedAndTOMLForms(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[skillopt\nanything = nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadOrg(paths); err != nil || cfg.Enabled() {
		t.Fatalf("unrelated malformed cfg=%+v err=%v", cfg, err)
	}
	content := "[org]\nenforce = 'block'\n[org.roles.'owner']\nscope = [\n  'owner/*', # comment\n]\nmerge_rule = 'owner'\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadOrg(paths)
	if err != nil {
		t.Fatal(err)
	}
	role, ok := cfg.Role("OWNER")
	if !ok || cfg.Enforce() != "block" || len(role.Scope) != 1 || role.Scope[0] != "owner/*" {
		t.Fatalf("cfg=%+v role=%+v ok=%v", cfg, role, ok)
	}
	for _, content := range []string{"[org\nenforce=\"block\"\n", "[ org.roles.owner\nscope=[\"*\"]\n", "[org .roles.x\nscope=[\"*\"]\n"} {
		if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrg(paths); err == nil {
			t.Fatalf("malformed org header unexpectedly succeeded: %q", content)
		}
	}
	// Unrelated sections whose name merely starts with "org" must stay tolerated
	// (the org-prefix must be a complete token), not brick dispatch.
	for _, content := range []string{
		"[organization\nx = 1\n", "[orgs\nx = 1\n", "[org_settings\nx = 1\n",
		"[organization]\nx = 1\n", "[orgchart]\nx = 1\n", "[org]]\nx = 1\n",
		"[[org]]\nenforce = \"block\"\n", "[[org.roles.\"owner\"]]\nscope = [\"*\"]\n",
	} {
		if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if cfg, err := LoadOrg(paths); err != nil || cfg.Enabled() {
			t.Fatalf("org-prefixed unrelated malformed header should be tolerated: %q cfg=%+v err=%v", content, cfg, err)
		}
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[ skillopt\nanything = nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadOrg(paths); err != nil || cfg.Enabled() {
		t.Fatalf("spaced unrelated malformed cfg=%+v err=%v", cfg, err)
	}
}

func TestLoadOrgAbsentAndInvalid(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if cfg, err := LoadOrg(paths); err != nil || cfg.Enabled() {
		t.Fatalf("absent cfg=%+v err=%v", cfg, err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"bad-role":   "[org.roles.\"Upper\"]\nscope=[\"*\"]\n",
		"empty-role": "[org.roles.\"\"]\nscope=[\"*\"]\n",
		"dangling":   "[org.roles.\"owner\"]\nscope=[\"*\"]\n[org.roles.\"child\"]\nparent=\"missing\"\nscope=[\"*\"]\n",
		"cycle":      "[org.roles.\"one\"]\nparent=\"two\"\nscope=[\"*\"]\n[org.roles.\"two\"]\nparent=\"one\"\nscope=[\"*\"]\n",
		"roots":      "[org.roles.\"one\"]\nscope=[\"*\"]\n[org.roles.\"two\"]\nscope=[\"*\"]\n",
		"rule":       "[org.roles.\"owner\"]\nscope=[\"*\"]\nmerge_rule=\"bad\"\n",
		"scope":      "[org.roles.\"owner\"]\nscope=[\"owner/repo/extra\"]\n",
		"wild-owner": "[org.roles.\"owner\"]\nscope=[\"*/*\"]\n",
		"subset":     "[org.roles.\"owner\"]\nscope=[\"one/*\"]\n[org.roles.\"child\"]\nparent=\"owner\"\nscope=[\"two/repo\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrg(paths); err == nil {
				t.Fatal("LoadOrg unexpectedly succeeded")
			}
		})
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org.roles.\"owner\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrg(paths); err == nil || !strings.Contains(err.Error(), "section") {
		t.Fatalf("malformed err=%v", err)
	}
}

func TestLoadOrgRejectsMalformedFieldsFailClosed(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	valid := "[org]\nenforce=\"block\"\n[org.roles.\"owner\"]\nscope=[\"*\"]\nmerge_rule=\"owner\"\n"
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "bad enforcement", body: "[org]\nenforce=\"open\"\n[org.roles.\"owner\"]\nscope=[\"*\"]\n", want: "org enforce"},
		{name: "unquoted role header", body: "[org.roles.owner]\nscope=[\"*\"]\n", want: "quoted TOML string"},
		{name: "bare roles header", body: "[org.roles]\nscope=[\"*\"]\n", want: "org role section"},
		{name: "empty first duplicate enforcement", body: "[org]\nenforce=\"\"\nenforce=\"block\"\n[org.roles.\"owner\"]\nscope=[\"*\"]\n", want: "duplicate [org].enforce"},
		{name: "duplicate org section", body: valid + "[org]\nenforce=\"warn\"\n", want: "duplicate [org]"},
		{name: "duplicate role section", body: valid + "[org.roles.\"owner\"]\nscope=[\"*\"]\n", want: "duplicate section"},
		{name: "unknown org field", body: "[org]\nenabled=true\n[org.roles.\"owner\"]\nscope=[\"*\"]\n", want: "unknown [org] field"},
		{name: "unknown role field", body: valid + "network=true\n", want: "unknown field"},
		{name: "duplicate role field", body: "[org.roles.\"owner\"]\nscope=[\"*\"]\nscope=[\"owner/*\"]\n", want: "duplicate field"},
		{name: "malformed role field", body: "[org.roles.\"owner\"]\nscope=[\"*\"]\nmerge_rule\n", want: "expected key = value"},
		{name: "empty scope", body: "[org.roles.\"owner\"]\nscope=[]\n", want: "scope must not be empty"},
		{name: "duplicate scope", body: "[org.roles.\"owner\"]\nscope=[\"*\",\"*\"]\n", want: "duplicate scope"},
		{name: "root not owner", body: "[org.roles.\"lead\"]\nscope=[\"*\"]\n", want: "root org role must be named"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(paths.ConfigFile, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadOrg(paths)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadOrg() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestLoadOrgOptionalOrgSectionEnforceAndMergeRule(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "roles without org section", body: "[org.roles.\"owner\"]\nscope=[\"*\"]\nmerge_rule=\"owner\"\n"},
		{name: "org without enforce", body: "[org]\n[org.roles.\"owner\"]\nscope=[\"*\"]\nmerge_rule=\"owner\"\n"},
		{name: "role without merge rule", body: "[org]\nenforce=\"block\"\n[org.roles.\"owner\"]\nscope=[\"*\"]\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(paths.ConfigFile, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadOrg(paths)
			if err != nil || !cfg.Enabled() || cfg.Enforce() != "block" {
				t.Fatalf("LoadOrg() cfg=%+v err=%v", cfg, err)
			}
		})
	}
}

func TestValidateOrgDeterministicRootErrorPrecedesParentReference(t *testing.T) {
	cfg := OrgConfig{roles: map[string]OrgRole{
		"a-child": {Name: "a-child", Parent: "missing", Scope: []string{"*"}},
		"lead":    {Name: "lead", Scope: []string{"*"}},
	}}
	const want = `root org role must be named "owner"; got "lead"`
	for range 100 {
		if err := ValidateOrg(cfg); err == nil || err.Error() != want {
			t.Fatalf("ValidateOrg() error = %v, want %q", err, want)
		}
	}
}

func TestScopeSubset(t *testing.T) {
	for _, tt := range []struct {
		child, parent []string
		want          bool
	}{
		{[]string{"one/repo"}, []string{"one/*"}, true}, {[]string{"one/*"}, []string{"*"}, true},
		{[]string{"one/*"}, []string{"two/*"}, false}, {[]string{"*"}, []string{"one/*"}, false},
	} {
		if got := scopeSubset(tt.child, tt.parent); got != tt.want {
			t.Fatalf("scopeSubset(%v,%v)=%v", tt.child, tt.parent, got)
		}
	}
}
