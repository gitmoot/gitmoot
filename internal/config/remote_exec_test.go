package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func remoteExecTestPaths(t *testing.T, content string) Paths {
	t.Helper()
	dir := t.TempDir()
	paths := PathsForHome(dir)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		if err := os.WriteFile(paths.ConfigFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func TestLoadRemoteExecConfigDefaultsToLocal(t *testing.T) {
	// A config file with no [remote_exec] section at all.
	paths := remoteExecTestPaths(t, "[workflow]\nimplement_base = \"main\"\n")
	cfg, err := LoadRemoteExecConfig(paths)
	if err != nil {
		t.Fatalf("LoadRemoteExecConfig: %v", err)
	}
	if cfg.Backend != "local" {
		t.Fatalf("Backend = %q, want the local default", cfg.Backend)
	}
}

func TestLoadRemoteExecConfigExplicitLocal(t *testing.T) {
	paths := remoteExecTestPaths(t, "[remote_exec]\nbackend = \"local\"\n")
	cfg, err := LoadRemoteExecConfig(paths)
	if err != nil {
		t.Fatalf("LoadRemoteExecConfig: %v", err)
	}
	if cfg.Backend != "local" {
		t.Fatalf("Backend = %q, want local", cfg.Backend)
	}
}

func TestLoadRemoteExecConfigUnknownFailsLoud(t *testing.T) {
	for _, value := range []string{"e2b", "loca"} {
		paths := remoteExecTestPaths(t, "[remote_exec]\nbackend = \""+value+"\"\n")
		_, err := LoadRemoteExecConfig(paths)
		if err == nil {
			t.Fatalf("backend %q loaded, want a loud error", value)
		}
		if !strings.Contains(err.Error(), "[remote_exec].backend") {
			t.Fatalf("backend %q error = %q, want the config key named", value, err)
		}
		if !strings.Contains(err.Error(), `"`+value+`"`) || !strings.Contains(err.Error(), "allowed: local") {
			t.Fatalf("backend %q error = %q, want the value AND the allowed set", value, err)
		}
	}
}

func TestLoadRemoteExecConfigExplicitBlankFailsLoud(t *testing.T) {
	for _, value := range []string{"", "   "} {
		paths := remoteExecTestPaths(t, "[remote_exec]\nbackend = \""+value+"\"\n")
		_, err := LoadRemoteExecConfig(paths)
		if err == nil {
			t.Fatalf("explicit blank backend %q loaded, want a loud error", value)
		}
		if !strings.Contains(err.Error(), "[remote_exec].backend") || !strings.Contains(err.Error(), `unknown execution backend ""`) || !strings.Contains(err.Error(), "allowed: local") {
			t.Fatalf("explicit blank backend %q error = %q, want key + blank value + allowed set", value, err)
		}
	}
}

func TestLoadRemoteExecConfigIgnoresUnknownKeysAndOtherSections(t *testing.T) {
	// A foreign "backend" key outside [remote_exec] must NOT be picked up, and
	// unknown keys inside the section stay forward-compatible.
	paths := remoteExecTestPaths(t, "[other]\nbackend = \"e2b\"\n\n[remote_exec]\nfuture_key = true\n")
	cfg, err := LoadRemoteExecConfig(paths)
	if err != nil {
		t.Fatalf("LoadRemoteExecConfig: %v", err)
	}
	if cfg.Backend != "local" {
		t.Fatalf("Backend = %q, want local (foreign section key must not leak)", cfg.Backend)
	}
}
