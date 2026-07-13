package config

import (
	"fmt"
	"os"
	"strings"
)

const (
	CredentialsGitHubDeny    = "deny"
	CredentialsGitHubInherit = "inherit"
)

// CredentialsConfig controls the off-by-default environment curation applied
// only to runtime-agent subprocesses. It is loaded when an adapter is built so
// each delivery observes the current file without daemon reload plumbing.
type CredentialsConfig struct {
	EnvCuration    bool
	EnvPassthrough []string
	GitHub         string
}

// DefaultCredentialsConfig preserves historical full-environment inheritance.
func DefaultCredentialsConfig() CredentialsConfig {
	return CredentialsConfig{GitHub: CredentialsGitHubDeny}
}

// LoadCredentialsConfig parses the optional [credentials] section.
func LoadCredentialsConfig(paths Paths) (CredentialsConfig, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return CredentialsConfig{}, err
	}
	cfg := DefaultCredentialsConfig()
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			current = strings.TrimSpace(section) == "credentials"
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
		case "env_curation":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return CredentialsConfig{}, fmt.Errorf("parse [credentials].env_curation: %w", err)
			}
			cfg.EnvCuration = parsed
		case "env_passthrough":
			parsed, err := parseConfigStringArray(value)
			if err != nil {
				return CredentialsConfig{}, fmt.Errorf("parse [credentials].env_passthrough: %w", err)
			}
			cfg.EnvPassthrough = parsed
		case "github":
			parsed, err := parseConfigString(value)
			if err != nil {
				return CredentialsConfig{}, fmt.Errorf("parse [credentials].github: %w", err)
			}
			cfg.GitHub = strings.TrimSpace(parsed)
		default:
			// Ignore unknown keys so the section remains forward-compatible.
		}
	}
	if err := validateCredentialsConfig(cfg); err != nil {
		return CredentialsConfig{}, err
	}
	return cfg, nil
}

func validateCredentialsConfig(cfg CredentialsConfig) error {
	switch cfg.GitHub {
	case CredentialsGitHubDeny, CredentialsGitHubInherit:
	default:
		return fmt.Errorf("unsupported [credentials].github %q; use deny or inherit", cfg.GitHub)
	}
	for _, pattern := range cfg.EnvPassthrough {
		if err := validateCredentialEnvPattern(pattern); err != nil {
			return fmt.Errorf("invalid [credentials].env_passthrough entry %q: %w", pattern, err)
		}
	}
	return nil
}

func validateCredentialEnvPattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("name must not be empty")
	}
	if strings.ContainsAny(pattern, "=\x00") {
		return fmt.Errorf("name must not contain '=' or NUL")
	}
	if index := strings.IndexByte(pattern, '*'); index >= 0 {
		if index != len(pattern)-1 || strings.Count(pattern, "*") != 1 {
			return fmt.Errorf("only a single trailing '*' glob is supported")
		}
	}
	return nil
}
