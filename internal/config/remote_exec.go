package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	// Backend is the [remote_exec].backend selection. "local" is the default;
	// "remote" is parseable while its provider factory remains a loud refusal.
	Backend string
	// LocalUID and LocalGID opt local-backend commands into an OS-level
	// privilege drop. They are a pair: omitting both preserves the daemon
	// identity, while setting only one is invalid.
	LocalUID *uint32
	LocalGID *uint32
	// LocalRoot optionally relocates local instances beneath a parent the
	// configured identity can traverse (required when the Gitmoot home itself is
	// below a root-only directory such as /root).
	LocalRoot string
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
		case "local_uid", "local_gid":
			parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
			if err != nil {
				return RemoteExecConfig{}, fmt.Errorf("parse [remote_exec].%s: expected an unsigned integer: %w", key, err)
			}
			converted := uint32(parsed)
			if key == "local_uid" {
				cfg.LocalUID = &converted
			} else {
				cfg.LocalGID = &converted
			}
		case "local_root":
			parsed, err := parseConfigString(value)
			if err != nil {
				return RemoteExecConfig{}, fmt.Errorf("parse [remote_exec].local_root: %w", err)
			}
			cfg.LocalRoot = strings.TrimSpace(parsed)
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
	if (cfg.LocalUID == nil) != (cfg.LocalGID == nil) {
		return fmt.Errorf("[remote_exec].local_uid and [remote_exec].local_gid must be configured together")
	}
	if cfg.LocalUID != nil {
		if *cfg.LocalUID == 0 || *cfg.LocalUID == ^uint32(0) {
			return fmt.Errorf("[remote_exec].local_uid must be a non-root usable uid, got %d", *cfg.LocalUID)
		}
		if *cfg.LocalGID == 0 || *cfg.LocalGID == ^uint32(0) {
			return fmt.Errorf("[remote_exec].local_gid must be a non-root usable gid, got %d", *cfg.LocalGID)
		}
	}
	if cfg.LocalRoot != "" && !filepath.IsAbs(cfg.LocalRoot) {
		return fmt.Errorf("[remote_exec].local_root must be an absolute path, got %q", cfg.LocalRoot)
	}
	if cfg.LocalRoot != "" && filepath.Dir(filepath.Clean(cfg.LocalRoot)) == filepath.Clean(cfg.LocalRoot) {
		return fmt.Errorf("[remote_exec].local_root must not be a filesystem root, got %q", cfg.LocalRoot)
	}
	return nil
}

// LocalIdentity returns nil unless the operator configured both identity
// fields. No account name or numeric identity is inferred from the host.
func (cfg RemoteExecConfig) LocalIdentity() *execbackend.LocalIdentity {
	if cfg.LocalUID == nil || cfg.LocalGID == nil {
		return nil
	}
	return &execbackend.LocalIdentity{UID: *cfg.LocalUID, GID: *cfg.LocalGID}
}
