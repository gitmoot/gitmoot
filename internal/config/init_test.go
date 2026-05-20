package config

import (
	"os"
	"strings"
	"testing"
)

func TestInitializeCreatesLocalState(t *testing.T) {
	paths := PathsForHome(t.TempDir())

	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	for _, dir := range []string{paths.Home, paths.Logs, paths.Workspaces} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("%s was not created as directory, info=%v err=%v", dir, info, err)
		} else if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 700", dir, info.Mode().Perm())
		}
	}

	config, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(config), "database") {
		t.Fatalf("config missing database path:\n%s", string(config))
	}
}
