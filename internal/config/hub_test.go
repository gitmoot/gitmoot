package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateHubListenAcceptsOnlyTailscaleAddresses(t *testing.T) {
	for _, ok := range []string{"100.106.218.88:7711", "[fd7a:115c:a1e0::8336:da59]:7711", "100.64.0.1:1"} {
		if _, err := ValidateHubListen(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"0.0.0.0:7711", "[::]:7711", "127.0.0.1:7711", "203.0.113.5:7711",
		"100.128.0.1:7711", // just outside 100.64.0.0/10
		"100.106.218.88:0", "100.106.218.88", "dev2306.tailnet.ts.net:7711", ":7711",
		"[::ffff:0.0.0.0]:7711",
	} {
		if _, err := ValidateHubListen(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestLoadHubConfig(t *testing.T) {
	load := func(content string) (HubConfig, error) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadHubConfig(Paths{ConfigFile: path})
	}
	cfg, err := load("[daemon]\nworkers = 2\n")
	if err != nil || cfg.Listen != "" {
		t.Fatalf("no [hub]: %+v %v", cfg, err)
	}
	cfg, err = load("[hub]\nlisten = \"100.106.218.88:7711\" # dev2306\nadvertise = [\"dev2306.tailnet.ts.net\"]\n")
	if err != nil || cfg.Listen != "100.106.218.88:7711" || len(cfg.Advertise) != 1 {
		t.Fatalf("valid [hub]: %+v %v", cfg, err)
	}
	for content, want := range map[string]string{
		"[hub]\nlisten = \"0.0.0.0:7711\"\n":                               "not a Tailscale address",
		"[hub]\nlisten = \"100.106.218.88:7711\"\nport = 1\n":              `unknown key "port"`,
		"[hub]\nadvertise = [\"x.ts.net\"]\n":                              "needs [hub].listen",
		"[hub\nlisten = \"0.0.0.0:7711\"\n":                                "malformed section header",
		"[hub]\nlisten = \"100.106.218.88:7711\"\nadvertise = [\"a b\"]\n": "not a host name",
	} {
		if _, err := load(content); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", content, err, want)
		}
	}
}

func TestLoadHubConfigAllow(t *testing.T) {
	load := func(content string) (HubConfig, error) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadHubConfig(Paths{ConfigFile: path})
	}
	cfg, err := load("[hub]\nlisten = \"100.106.218.88:7711\"\n")
	if err != nil || strings.Join(cfg.Allow, ",") != "messages,notifications" {
		t.Fatalf("absent allow: %v %v", cfg.Allow, err)
	}
	cfg, err = load("[hub]\nlisten = \"100.106.218.88:7711\"\nallow = []\n")
	if err != nil || cfg.Allow == nil || len(cfg.Allow) != 0 {
		t.Fatalf("empty allow must stay empty, not fall back to the default: %#v %v", cfg.Allow, err)
	}
	cfg, err = load("[hub]\nlisten = \"100.106.218.88:7711\"\nallow = [\"Reviews\", \"messages\", \"reviews\"]\n")
	if err != nil || strings.Join(cfg.Allow, ",") != "reviews,messages" {
		t.Fatalf("listed allow: %v %v", cfg.Allow, err)
	}
	if _, err := load("[hub]\nlisten = \"100.106.218.88:7711\"\nallow = [\"admin\"]\n"); err == nil || !strings.Contains(err.Error(), `unknown group "admin"`) {
		t.Fatalf("unknown group: %v", err)
	}
}
