package org

import (
	"reflect"
	"strings"
	"testing"
)

const validRegistry = `
[org]
enforce = "block"

[org.roles."owner"]
scope = ["*"]
merge_rule = "owner"

[org.roles."platform"]
parent = "owner"
scope = ["gitmoot/*", "*/docs"]
merge_rule = "parent"

[org.roles."api"]
parent = "platform"
scope = ["gitmoot/api"]
merge_rule = "review"
`

func TestParseRegistryValidHierarchyAndScope(t *testing.T) {
	registry, err := ParseRegistry([]byte(validRegistry))
	if err != nil {
		t.Fatalf("ParseRegistry() error = %v", err)
	}
	if !registry.Enabled() || registry.Enforce != EnforceBlock || len(registry.Roles) != 3 {
		t.Fatalf("registry = %+v", registry)
	}
	if got := registry.Path("api"); !reflect.DeepEqual(got, []string{"owner", "platform", "api"}) {
		t.Fatalf("Path(api) = %v", got)
	}
	if got := registry.Children("owner"); len(got) != 1 || got[0].Name != "platform" {
		t.Fatalf("Children(owner) = %+v", got)
	}
	if !registry.Matches("platform", "gitmoot/cli") || !registry.Matches("platform", "other/docs") || registry.Matches("platform", "other/api") {
		t.Fatalf("scope matching did not honor configured patterns")
	}
}

func TestParseRegistryAbsentIsDisabled(t *testing.T) {
	tests := []string{
		"[workflow]\nresult_checks = \"warn\"\n[[plugins]]\nname = \"org-helper\"\n",
		"[organization]\nenabled = true\n",
		"[orgchart]\nlayout = \"tree\"\n",
		"[org]]\nenforce = \"block\"\n",
		"[org\nenforce = \"block\"\n",
		"[[org]]\nenforce = \"block\"\n",
		"[[org.roles.\"owner\"]]\nscope = [\"*\"]\n",
	}
	for _, body := range tests {
		registry, err := ParseRegistry([]byte(body))
		if err != nil {
			t.Errorf("ParseRegistry(%q) error = %v", body, err)
			continue
		}
		if registry.Enabled() {
			t.Errorf("ParseRegistry(%q) unexpectedly enabled: %+v", body, registry)
		}
	}
}

func TestParseRegistryRejectsMalformedFailClosed(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "bad enforcement", body: strings.Replace(validRegistry, `"block"`, `"open"`, 1), want: "enforce"},
		{name: "unquoted role header", body: strings.Replace(validRegistry, `[org.roles."platform"]`, `[org.roles.platform]`, 1), want: "section"},
		{name: "bad org header", body: "[org.roles]\nname = \"x\"\n", want: "org role section"},
		{name: "empty first duplicate enforcement", body: "[org]\nenforce = \"\"\nenforce = \"block\"\n[org.roles.\"owner\"]\nscope = [\"*\"]\nmerge_rule = \"owner\"\n", want: "duplicate [org].enforce"},
		{name: "duplicate org", body: validRegistry + "\n[org]\nenforce = \"warn\"\n", want: "duplicate [org]"},
		{name: "duplicate role", body: validRegistry + "\n[org.roles.\"api\"]\nscope = [\"*\"]\nmerge_rule = \"x\"\n", want: "duplicate org role"},
		{name: "unknown field", body: strings.Replace(validRegistry, `merge_rule = "review"`, "merge_rule = \"review\"\nnetwork = true", 1), want: "unknown org role field"},
		{name: "missing parent", body: strings.Replace(validRegistry, `parent = "platform"`, `parent = "missing"`, 1), want: "undeclared parent"},
		{name: "multiple roots", body: strings.Replace(validRegistry, `parent = "owner"`, "", 1), want: "root org role"},
		{name: "root not owner", body: strings.Replace(validRegistry, `"owner"`, `"lead"`, 1), want: "root org role"},
		{name: "cycle", body: strings.Replace(validRegistry, "[org.roles.\"owner\"]\nscope", "[org.roles.\"owner\"]\nparent = \"api\"\nscope", 1), want: "exactly one root"},
		{name: "scope broadens owner", body: strings.Replace(validRegistry, `scope = ["*"]`, `scope = ["gitmoot/*"]`, 1), want: "not a subset"},
		{name: "scope broadens repo", body: strings.Replace(validRegistry, `scope = ["gitmoot/api"]`, `scope = ["other/api"]`, 1), want: "not a subset"},
		{name: "invalid scope", body: strings.Replace(validRegistry, `scope = ["gitmoot/api"]`, `scope = ["gitmoot"]`, 1), want: "must be"},
		{name: "empty scope", body: strings.Replace(validRegistry, `scope = ["gitmoot/api"]`, `scope = []`, 1), want: "non-empty scope"},
		{name: "malformed field", body: strings.Replace(validRegistry, `merge_rule = "review"`, "merge_rule", 1), want: "malformed org field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseRegistry([]byte(test.body))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseRegistry() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestParseRegistryOptionalFieldsMatchConfigLoadOrg(t *testing.T) {
	// #1057/#1073 reconcile: the [org] section, its enforce key, and a role's
	// merge_rule are all OPTIONAL — matching the authoritative config.LoadOrg
	// acceptance rule (enforce defaults to "block"; merge_rule is advisory in
	// phase 1a). The presence of any [org.roles.*] enables the registry.
	// Genuinely malformed input still fails closed (covered by the reject table).
	for _, tc := range []struct{ name, body string }{
		{"roles without [org] section", strings.Replace(validRegistry, "[org]\nenforce = \"block\"\n", "", 1)},
		{"[org] without enforce key", strings.Replace(validRegistry, `enforce = "block"`, "", 1)},
		{"role without merge_rule (advisory/optional)", strings.Replace(validRegistry, `merge_rule = "review"`, "", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, err := ParseRegistry([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseRegistry() error = %v, want valid", err)
			}
			if !reg.Enabled() {
				t.Fatal("registry not enabled; any [org.roles.*] must enable it")
			}
			if reg.Enforce != EnforceBlock {
				t.Fatalf("Enforce = %q, want default %q", reg.Enforce, EnforceBlock)
			}
		})
	}
}

func TestRegistryPathTerminatesOnDirectCycle(t *testing.T) {
	registry := Registry{Roles: map[string]Role{
		"a": {Name: "a", Parent: "b"},
		"b": {Name: "b", Parent: "a"},
	}}
	if got := registry.Path("a"); len(got) == 0 || len(got) > len(registry.Roles) {
		t.Fatalf("Path(a) = %v, want a bounded non-empty path", got)
	}
}

func TestScopeMatchesFrozenGrammar(t *testing.T) {
	tests := []struct {
		pattern string
		repo    string
		want    bool
	}{
		{"*", "gitmoot/gitmoot", true},
		{"gitmoot/*", "gitmoot/gitmoot", true},
		{"gitmoot/*", "other/gitmoot", false},
		{"*/gitmoot", "other/gitmoot", true},
		{"gitmoot/gitmoot", "gitmoot/gitmoot", true},
		{"*/*", "gitmoot/gitmoot", false},
		{"gitmoot", "gitmoot/gitmoot", false},
	}
	for _, test := range tests {
		if got := ScopeMatches(test.pattern, test.repo); got != test.want {
			t.Errorf("ScopeMatches(%q, %q) = %v, want %v", test.pattern, test.repo, got, test.want)
		}
	}
}
