package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/gitmoot/gitmoot/internal/execbackend"
)

// RemoteExecConfig is the optional [remote_exec] section (#1535 P0 contract,
// #1536 P1): it selects the execution backend — WHERE a job's runtime
// subprocess executes. It is distinct from the Landlock local-confinement
// surface (internal/sandbox, the `sandbox` CLI, agent path grants), which is
// unrelated and untouched by this seam.
//
// The section is OFF by default: a config file with no [remote_exec] section
// loads the DefaultRemoteExecConfig ("local"), which is a byte-for-byte
// passthrough to the pre-#1536 runner composition.
type RemoteExecConfig struct {
	// Backend is the [remote_exec].backend selection. "local" is the default
	// and the only implemented value; anything else fails validation loud.
	Backend string
}

// DefaultRemoteExecConfig preserves today's behaviour: the local backend.
func DefaultRemoteExecConfig() RemoteExecConfig {
	return RemoteExecConfig{Backend: string(execbackend.Local)}
}

// LoadRemoteExecConfig parses the optional [remote_exec] section. A missing
// section returns the default (local). An unknown backend value is a hard
// error — via execbackend.Parse it names the offending value AND the allowed
// set, and dispatch surfaces it as a job failure rather than ever falling
// back silently.
func LoadRemoteExecConfig(paths Paths) (RemoteExecConfig, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return RemoteExecConfig{}, err
	}
	cfg := DefaultRemoteExecConfig()
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			current = strings.TrimSpace(section) == "remote_exec"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "backend":
			parsed, err := parseConfigString(value)
			if err != nil {
				return RemoteExecConfig{}, fmt.Errorf("parse [remote_exec].backend: %w", err)
			}
			cfg.Backend = strings.TrimSpace(parsed)
		default:
			// Ignore unknown keys so the section remains forward-compatible.
		}
	}
	if err := validateRemoteExecConfig(cfg); err != nil {
		return RemoteExecConfig{}, err
	}
	return cfg, nil
}

func validateRemoteExecConfig(cfg RemoteExecConfig) error {
	if _, err := execbackend.ParseImplemented(cfg.Backend); err != nil {
		return fmt.Errorf("unsupported [remote_exec].backend: %w", err)
	}
	return nil
}
