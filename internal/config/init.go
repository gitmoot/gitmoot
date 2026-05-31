package config

import (
	"fmt"
	"os"
)

func Initialize(paths Paths) error {
	for _, dir := range []string{paths.Home, paths.Logs, paths.Workspaces, paths.Evals, paths.ArtifactBlobs} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod %s: %w", dir, err)
		}
	}

	if _, err := os.Stat(paths.ConfigFile); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat config file: %w", err)
	}

	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)), 0o600); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	return nil
}

func DefaultConfig(paths Paths) string {
	return fmt.Sprintf(`# Gitmoot local configuration.

[paths]
database = %q
logs = %q
workspaces = %q
evals = %q
artifact_blobs = %q
`, paths.Database, paths.Logs, paths.Workspaces, paths.Evals, paths.ArtifactBlobs)
}
