package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gitmoot/gitmoot/internal/config"
)

func TestResolveConfigFileEmptyHomeUsesDefaultConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte("[daemon]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := resolveConfigFile(""); got != paths.ConfigFile {
		t.Fatalf("resolveConfigFile(empty) = %q, want default config %q", got, paths.ConfigFile)
	}
}

func TestPermissionPolicyObservationEmptyHomeReadsConfiguredValue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o755); err != nil {
		t.Fatal(err)
	}
	contents := "[daemon]\npermission_policy_observation_enabled = true\n"
	if err := os.WriteFile(paths.ConfigFile, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	if !resolvePermissionPolicyObservationEnabled("") {
		t.Fatal("empty home ignored configured permission_policy_observation_enabled=true")
	}
}

func TestResolveConfigFileEmptyHomeMissingConfigUsesDefaultsWithoutCreatingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}

	if got := resolveConfigFile(""); got != paths.ConfigFile {
		t.Fatalf("resolveConfigFile(empty) = %q, want missing default config path %q", got, paths.ConfigFile)
	}
	if resolvePermissionPolicyObservationEnabled("") {
		t.Fatal("missing config enabled permission-policy observation")
	}
	if _, err := os.Stat(paths.ConfigFile); !os.IsNotExist(err) {
		t.Fatalf("default config stat error = %v, want not-exist without creating a file", err)
	}
}
