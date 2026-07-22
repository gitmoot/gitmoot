package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadOrgRegistry(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	content := `[org]
enforce = "warn"
[org.roles."owner"]
scope = ["*"]
merge_rule = "owner"
`
	if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := LoadOrgRegistry(paths)
	if err != nil {
		t.Fatalf("LoadOrgRegistry() error = %v", err)
	}
	if !registry.Enabled() || registry.Enforce != "warn" {
		t.Fatalf("registry = %+v", registry)
	}
}

func TestLoadOrgRegistryMalformedFailsClosed(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[org]\nenforce = \"maybe\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrgRegistry(paths)
	if err == nil || !strings.Contains(err.Error(), "load org registry") {
		t.Fatalf("LoadOrgRegistry() error = %v", err)
	}
}

func TestLoadOrgRegistryMissingIsDisabled(t *testing.T) {
	registry, err := LoadOrgRegistry(PathsForHome(t.TempDir()))
	if err != nil {
		t.Fatalf("LoadOrgRegistry() error = %v", err)
	}
	if registry.Enabled() {
		t.Fatalf("registry unexpectedly enabled: %+v", registry)
	}
}
