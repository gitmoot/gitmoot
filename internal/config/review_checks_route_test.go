package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeReviewChecksConfig(t *testing.T, body string) Paths {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "e2b.key")
	if err := os.WriteFile(key, []byte("e2b-test-key-0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := "[remote_exec]\ne2b_api_key_file = \"" + key + "\"\ne2b_template = \"base\"\n\n" + body
	file := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return Paths{ConfigFile: file}
}

func TestReviewChecksRouteLoadsAndValidates(t *testing.T) {
	paths := writeReviewChecksConfig(t, `[repos."o/ok".review]
checks_backend = "remote"
checks_template = "swift-tmpl"
remote_routing_enabled = true

[repos."o/unknown".review]
checks_backend = "remote"
checks_provider = "gcp"

[repos."o/sandboxd".review]
checks_backend = "remote"
checks_provider = "sandboxd"

[repos."o/mac".review]
checks_backend = "remote"
checks_provider = "mac"

[repos."o/nobackend".review]
checks_template = "x"
`)
	cfg, err := LoadReviewConfig(paths)
	if err != nil {
		t.Fatalf("checks routing errors must stay per repository, got load error %v", err)
	}
	route, err := cfg.ChecksRoute("o/ok")
	if err != nil || !route.Enabled() || route.Provider != RemoteExecProviderE2B || route.Template != "swift-tmpl" {
		t.Fatalf("o/ok route = %+v, %v", route, err)
	}
	if !cfg.For("o/ok").RemoteRoutingEnabled {
		t.Fatal("checks keys must not disturb the repository's other review fields")
	}
	for repo, want := range map[string]string{
		"o/unknown":   "unknown checks_provider",
		"o/sandboxd":  "[remote_exec.sandboxd]",
		"o/mac":       "[remote_exec.sandboxd]",
		"o/nobackend": "require checks_backend",
	} {
		if _, err := cfg.ChecksRoute(repo); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s route error = %v; want %q", repo, err, want)
		}
	}
	if route, err := cfg.ChecksRoute("o/unconfigured"); err != nil || route.Enabled() {
		t.Fatalf("unconfigured repo route = %+v, %v", route, err)
	}
	// checks_provider = "mac" is the deprecated alias: one warning, for that
	// repository only, naming the replacement.
	if got := cfg.Deprecations(); len(got) != 1 || !strings.Contains(got[0], `"o/mac"`) || !strings.Contains(got[0], `use "sandboxd"`) {
		t.Fatalf("deprecations = %q; want one for o/mac naming sandboxd", got)
	}
}

func TestReviewChecksKeysAreRepositoryScoped(t *testing.T) {
	paths := writeReviewChecksConfig(t, "[review]\nchecks_backend = \"remote\"\n")
	if _, err := LoadReviewConfig(paths); err == nil || !strings.Contains(err.Error(), "repository-scoped") {
		t.Fatalf("global checks_backend error = %v", err)
	}
}

func TestRemoteExecCostPerHourParses(t *testing.T) {
	paths := writeReviewChecksConfig(t, "")
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.Replace(string(content), "[remote_exec]\n", "[remote_exec]\ncost_per_hour_usd = 0.166\n", 1))
	if err := os.WriteFile(paths.ConfigFile, content, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRemoteExecConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExecBackendCost.PerHourUSD != 0.166 {
		t.Fatalf("PerHourUSD = %v", cfg.ExecBackendCost.PerHourUSD)
	}
}
