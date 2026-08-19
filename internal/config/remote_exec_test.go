package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitmoot/gitmoot/internal/execbackend"
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
	if cfg.LocalIdentity() != nil || cfg.LocalRoot != "" {
		t.Fatalf("default local privilege config = uid %v gid %v root %q, want unset", cfg.LocalUID, cfg.LocalGID, cfg.LocalRoot)
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

func TestLoadRemoteExecConfigLocalIdentity(t *testing.T) {
	paths := remoteExecTestPaths(t, "[remote_exec]\nbackend = \"local\"\nlocal_uid = 1234\nlocal_gid = 5678\nlocal_root = \"/var/tmp/gitmoot-local\"\n")
	cfg, err := LoadRemoteExecConfig(paths)
	if err != nil {
		t.Fatalf("LoadRemoteExecConfig: %v", err)
	}
	identity := cfg.LocalIdentity()
	if identity == nil || identity.UID != 1234 || identity.GID != 5678 {
		t.Fatalf("LocalIdentity = %+v, want uid 1234 gid 5678", identity)
	}
	if cfg.LocalRoot != "/var/tmp/gitmoot-local" {
		t.Fatalf("LocalRoot = %q", cfg.LocalRoot)
	}
}

func TestLoadRemoteExecConfigRejectsIncompleteOrRootIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{name: "uid only", content: "local_uid = 1234\n", want: "configured together"},
		{name: "gid only", content: "local_gid = 1234\n", want: "configured together"},
		{name: "root uid", content: "local_uid = 0\nlocal_gid = 1234\n", want: "local_uid must be a non-root"},
		{name: "root gid", content: "local_uid = 1234\nlocal_gid = 0\n", want: "local_gid must be a non-root"},
		{name: "relative root", content: "local_root = \"relative\"\n", want: "must be an absolute path"},
		{name: "filesystem root", content: "local_root = \"/\"\n", want: "must not be a filesystem root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := remoteExecTestPaths(t, "[remote_exec]\nbackend = \"local\"\n"+tc.content)
			_, err := LoadRemoteExecConfig(paths)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadRemoteExecConfig error = %v, want %q", err, tc.want)
			}
		})
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

func TestLoadRemoteExecConfigAdvertisedWithoutImplementationFailsLoud(t *testing.T) {
	original := append([]string(nil), execbackend.AllowedNames...)
	defer func() { execbackend.AllowedNames = original }()
	execbackend.AllowedNames = append(execbackend.AllowedNames, "future-remote")

	paths := remoteExecTestPaths(t, "[remote_exec]\nbackend = \"future-remote\"\n")
	_, err := LoadRemoteExecConfig(paths)
	if err == nil || !strings.Contains(err.Error(), `"future-remote"`) || !strings.Contains(err.Error(), "advertised but not implemented") {
		t.Fatalf("advertised future backend error = %v, want loud missing-implementation error", err)
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
