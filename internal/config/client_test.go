package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadClientConfig(t *testing.T) {
	load := func(content string) (ClientConfig, error) {
		home := t.TempDir()
		path := filepath.Join(home, "config.toml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadClientConfig(Paths{Home: home, ConfigFile: path})
	}
	if cfg, err := load("[daemon]\nworkers = 1\n"); err != nil || cfg.Enabled() {
		t.Fatalf("no [client]: %+v %v", cfg, err)
	}
	cfg, err := load("[client]\nhub = \"https://100.106.218.88:7711/\"\ncerts = \"hub-certs\"\nname = \"Mac-Studio\"\nagent = \"mac-studio\"\n")
	if err != nil || cfg.Hub != "https://100.106.218.88:7711" || cfg.Name != "mac-studio" || !filepath.IsAbs(cfg.Certs) || cfg.Agent != "mac-studio" {
		t.Fatalf("valid [client]: %+v %v", cfg, err)
	}
	for content, want := range map[string]string{
		"[client]\nhub = \"http://100.106.218.88:7711\"\ncerts = \"c\"\nname = \"m\"\n":    "want https://HOST:PORT",
		"[client]\nhub = \"https://100.106.218.88\"\ncerts = \"c\"\nname = \"m\"\n":        "want https://HOST:PORT",
		"[client]\nhub = \"https://100.106.218.88:7711/x\"\ncerts = \"c\"\nname = \"m\"\n": "want https://HOST:PORT",
		"[client]\nhub = \"https://100.106.218.88:7711\"\nname = \"m\"\n":                  "needs certs and name",
		"[client]\ncerts = \"c\"\n":                                       "needs hub",
		"[client]\nhub = \"https://100.106.218.88:7711\"\nport = \"1\"\n": `unknown key "port"`,
		"[client\nhub = \"https://100.106.218.88:7711\"\n":                "malformed section header",
	} {
		if _, err := load(content); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", content, err, want)
		}
	}
}
