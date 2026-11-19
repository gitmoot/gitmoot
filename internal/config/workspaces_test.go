package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadWorkspacesPolicyDefaultsAreDisabledWithPilotCap(t *testing.T) {
	policy, err := LoadWorkspacesPolicy(writeGateConfig(t, "[remote_exec]\nbackend = \"local\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := WorkspacesPolicy{Provider: "e2b", MaxTotalUSD: 50, MaxWorkspaceUSD: 10, IdleTimeout: 4 * time.Hour}
	if policy != want {
		t.Fatalf("policy = %+v, want %+v", policy, want)
	}
}

func TestLoadWorkspacesPolicyRefusesInvalidValues(t *testing.T) {
	for _, body := range []string{
		"[workspaces]\nmax_total_usd = 0\n",
		"[workspaces]\nmax_total_usd = -5\n",
		"[workspaces]\nmax_workspace_usd = lots\n",
		"[workspaces]\nmax_total_usd = 5\nmax_workspace_usd = 6\n",
		"[workspaces]\nidle_timeout = \"30s\"\n",
		"[workspaces]\nenabled = yes\n",
		"[workspaces]\nprovider = \"aws\"\n",
		"[workspaces]\nmax_total = 5\n",
		"[workspaces\nenabled = true\n",
		"[workspaces]\ntemplate = \"\"\n",
		"[workspaces]\ntemplate = \"has space\"\n",
		"[workspaces]\ntemplate = \"-leading-dash\"\n",
		"[workspaces]\ntemplate = \"quote\\\"d\"\n",
		"[workspaces]\ntemplate = base\n",
	} {
		if _, err := LoadWorkspacesPolicy(writeGateConfig(t, body)); err == nil {
			t.Fatalf("LoadWorkspacesPolicy accepted %q", body)
		}
	}
	policy, err := LoadWorkspacesPolicy(writeGateConfig(t, "[workspaces]\nenabled = true\nprovider = \"sandboxd\"\nmax_total_usd = 20\nmax_workspace_usd = 4.5\nidle_timeout = \"90m\"\n[disk_guard\nmin_free_bytes = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := WorkspacesPolicy{Enabled: true, Provider: "sandboxd", MaxTotalUSD: 20, MaxWorkspaceUSD: 4.5, IdleTimeout: 90 * time.Minute}
	if policy != want {
		t.Fatalf("policy = %+v, want %+v", policy, want)
	}
	if policy, err := LoadWorkspacesPolicy(writeGateConfig(t, "[workspaces]\nprovider = \"local\"\n")); err != nil || policy.Provider != WorkspacesLocalProvider {
		t.Fatalf("local provider = %+v %v", policy, err)
	}
}

func TestLoadWorkspacesPolicyTemplate(t *testing.T) {
	for _, name := range []string{"gitmoot-omp-go126-2c4g-v1", "rki5dems9wqfm4r03t7g", "team/name:v1.2_3"} {
		policy, err := LoadWorkspacesPolicy(writeGateConfig(t, "[workspaces]\ntemplate = \""+name+"\"\n"))
		if err != nil || policy.Template != name {
			t.Fatalf("template %q = %+v, %v", name, policy, err)
		}
	}
	// Unset means the provider's [remote_exec] template (resolved by the CLI).
	if policy, err := LoadWorkspacesPolicy(writeGateConfig(t, "[workspaces]\nenabled = true\n")); err != nil || policy.Template != "" {
		t.Fatalf("no template = %+v, %v", policy, err)
	}
}

func TestLoadRepoWorkspaces(t *testing.T) {
	cfg, err := LoadRepoWorkspaces(writeGateConfig(t, `[workspaces]
template = "global-tmpl"

[repos."Acme/Widget".workspace]
template = "team/widget:v2"
require = ["go version", "test -f go.mod && echo \"a,b\"" ]

[repos."acme/widget".review]
require = ["not this one"]

[repos."acme/other".workspace]
require = ["make -v",]
`))
	if err != nil {
		t.Fatal(err)
	}
	// Repository names match case-insensitively.
	for _, repo := range []string{"acme/widget", "ACME/WIDGET", " Acme/Widget "} {
		got, err := cfg.For(repo)
		if err != nil || got.Template != "team/widget:v2" || !reflect.DeepEqual(got.Require, []string{"go version", `test -f go.mod && echo "a,b"`}) {
			t.Fatalf("For(%q) = %+v, %v", repo, got, err)
		}
	}
	if got, err := cfg.For("acme/other"); err != nil || got.Template != "" || !reflect.DeepEqual(got.Require, []string{"make -v"}) {
		t.Fatalf("acme/other = %+v, %v", got, err)
	}
	if got, err := cfg.For("acme/none"); err != nil || got.Template != "" || got.Require != nil {
		t.Fatalf("a repository without a table = %+v, %v", got, err)
	}
	if got, err := LoadRepoWorkspaces(writeGateConfig(t, "[workspaces]\nenabled = true\n")); err != nil || len(got.repos) != 0 {
		t.Fatalf("no tables = %+v, %v", got, err)
	}
}

func TestLoadRepoWorkspacesRefusesInvalidValues(t *testing.T) {
	sixteen := strings.TrimSuffix(strings.Repeat(`"true", `, MaxWorkspaceRequire), ", ")
	for _, body := range []string{
		`template = ""`,
		`template = "has space"`,
		`template = "-leading-dash"`,
		`template = base`,
		`require = "go version"`,
		`require = [""]`,
		`require = ["go version", "   "]`,
		`require = ["\t"]`,
		`require = [go]`,
		`require = ["go" "make"]`,
		`require = ["unterminated]`,
		`require = ["nul\u0000byte"]`,
		`require = ["go version",`,
		`require = [` + sixteen + `, "one too many"]`,
	} {
		cfg, err := LoadRepoWorkspaces(writeGateConfig(t, "[repos.\"acme/widget\".workspace]\n"+body+"\n[repos.\"acme/other\".workspace]\nrequire = [\"true\"]\n"))
		if err != nil {
			t.Fatalf("%q refused the whole file: %v", body, err)
		}
		if got, err := cfg.For("Acme/Widget"); err == nil {
			t.Fatalf("[repos.\"acme/widget\".workspace] %s accepted: %+v", body, got)
		}
		// One repository's invalid table never refuses another's.
		if got, err := cfg.For("acme/other"); err != nil || len(got.Require) != 1 {
			t.Fatalf("after %q acme/other = %+v, %v", body, got, err)
		}
	}
	cfg, err := LoadRepoWorkspaces(writeGateConfig(t, "[repos.\"acme/widget\".workspace]\nrequire = ["+sixteen+"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.For("acme/widget"); err != nil || len(got.Require) != MaxWorkspaceRequire {
		t.Fatalf("%d checks = %+v, %v", MaxWorkspaceRequire, got, err)
	}
}

func TestLoadRepoWorkspacesRefusesMalformedHeader(t *testing.T) {
	if _, err := LoadRepoWorkspaces(writeGateConfig(t, "[repos.\"acme/widget\".workspace\nrequire = [\"true\"]\n")); err == nil {
		t.Fatal("LoadRepoWorkspaces accepted a malformed [repos.\"o/r\".workspace header")
	}
	// A typo in an unrelated section is not this loader's to refuse.
	if _, err := LoadRepoWorkspaces(writeGateConfig(t, "[disk_guard\nmin_free_bytes = 1\n[workspaces\n")); err != nil {
		t.Fatalf("an unrelated malformed header refused: %v", err)
	}
}

// A closed workspace header whose repository will not read is refused for the
// whole file, never skipped: which repository's checks it meant is unknown.
func TestLoadRepoWorkspacesRefusesUnreadableRepositoryHeader(t *testing.T) {
	for _, header := range []string{
		`[repos."acme/widget.workspace]`, // the review's probe
		`[repos.acme/widget".workspace]`,
		`[repos."acme/widget\q".workspace]`,
		`[repos."".workspace]`,
		`[repos. .workspace]`,
	} {
		cfg, err := LoadRepoWorkspaces(writeGateConfig(t, header+"\nrequire = [\"go version\"]\n\n[repos.\"acme/other\".workspace]\nrequire = [\"make -v\"]\n"))
		if err == nil {
			got, forErr := cfg.For("acme/widget")
			t.Fatalf("%s loaded: acme/widget = %+v, %v", header, got, forErr)
		}
	}
	// Not workspace tables: left to their own loaders.
	for _, header := range []string{`[repos.workspace]`, `[repos."acme/widget".workspaces]`, `[repos."acme/widget.review]`} {
		if _, err := LoadRepoWorkspaces(writeGateConfig(t, header+"\nrequire = [\"go version\"]\n")); err != nil {
			t.Fatalf("%s refused: %v", header, err)
		}
	}
}
