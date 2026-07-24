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
	defaultDir, err := defaultToolCacheDir(paths.Home)
	if err != nil {
		return ToolCachePolicy{}, err
	}
	policy := ToolCachePolicy{Enabled: true, Dir: defaultDir}
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
		if strings.HasPrefix(line, "[") {
			if strings.HasSuffix(line, "]") {
				current = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			} else {
				// A malformed header (no closing bracket) ends whatever section
				// was active rather than silently continuing it — otherwise a
				// botched "[workflow" typo right after "[cache]" would keep
				// applying subsequent lines to [cache] instead (#1113 finder).
				current = ""
			}
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
				policy.Dir = defaultDir
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

// defaultToolCacheDir joins home with the default subdir and normalizes the
// result to an absolute path: pathsFromFlag does not itself require --home to
// be absolute, so home (and therefore this join) can be relative. A relative
// Dir is rejected outright by Landlock (produce jobs) and, for unsandboxed
// jobs, would resolve against the CHILD PROCESS's cwd (the worktree checkout)
// rather than the daemon's — a different absolute path per worktree, silently
// recreating the exact per-worktree cache duplication this feature exists to
// eliminate. filepath.Abs only errors if the working directory is
// unresolvable, which is already fatal for the daemon process itself.
func defaultToolCacheDir(home string) (string, error) {
	dir := filepath.Join(home, filepath.FromSlash(ToolCacheDefaultSubdir))
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve default tool cache dir: %w", err)
	}
	return abs, nil
}
