package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ToolCacheDefaultSubdir is the tool-cache root's location under the gitmoot
// home when [cache].dir is not overridden.
const ToolCacheDefaultSubdir = "cache/tools"

// ToolCachePolicy controls whether isolated-worktree jobs get a shared,
// host-level tool-cache directory injected into their environment (#1113).
type ToolCachePolicy struct {
	Enabled bool
	// Dir is always absolute and non-empty when Enabled is true.
	Dir string
}

// LoadToolCache resolves the independent [cache] section: enabled (default
// true) and dir (default "<home>/cache/tools"). An explicit dir override must
// be absolute. enabled=false disables the feature entirely regardless of dir.
func LoadToolCache(paths Paths) (ToolCachePolicy, error) {
	policy := ToolCachePolicy{Enabled: true, Dir: filepath.Join(paths.Home, filepath.FromSlash(ToolCacheDefaultSubdir))}
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return policy, nil
		}
		return ToolCachePolicy{}, err
	}
	current := ""
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if current != "cache" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			value, err = parseConfigString(value)
			if err != nil {
				return ToolCachePolicy{}, fmt.Errorf("parse [cache].%s: %w", key, err)
			}
			value = strings.TrimSpace(value)
		}
		switch key {
		case "enabled":
			switch value {
			case "", "true":
				policy.Enabled = true
			case "false":
				policy.Enabled = false
			default:
				return ToolCachePolicy{}, fmt.Errorf("invalid [cache].enabled %q: must be true or false", value)
			}
		case "dir":
			if value == "" {
				policy.Dir = filepath.Join(paths.Home, filepath.FromSlash(ToolCacheDefaultSubdir))
				continue
			}
			if !filepath.IsAbs(value) {
				return ToolCachePolicy{}, fmt.Errorf("[cache].dir %q must be absolute", value)
			}
			policy.Dir = value
		}
	}
	return policy, nil
}
