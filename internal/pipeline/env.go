package pipeline

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

var pipelineEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func ValidateEnvName(name string) error {
	if !pipelineEnvNamePattern.MatchString(name) {
		return fmt.Errorf("must match [A-Za-z_][A-Za-z0-9_]*")
	}
	return nil
}

func ReservedEnvName(name string) bool {
	return strings.HasPrefix(name, "GITMOOT_")
}

// ValidateEnvSelector accepts an exact name or a path.Match-style glob over
// names (for example REDDIT_*).
func ValidateEnvSelector(selector string) error {
	if selector == "" || strings.ContainsAny(selector, "/\\") {
		return fmt.Errorf("must be an environment name or glob")
	}
	if !strings.ContainsAny(selector, "*?[") {
		return ValidateEnvName(selector)
	}
	if _, err := path.Match(selector, "ENV_NAME"); err != nil {
		return fmt.Errorf("invalid glob: %w", err)
	}
	for _, r := range selector {
		if r == '*' || r == '?' || r == '[' || r == ']' || r == '-' ||
			(r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return fmt.Errorf("must be an environment name or glob")
	}
	return nil
}

func ReservedEnvSelector(selector string) bool {
	if ReservedEnvName(selector) {
		return true
	}
	matched, err := path.Match(selector, "GITMOOT_INTERNAL")
	return err == nil && matched
}

// ParseEnv parses blank lines, comments, optional "export ", and one
// KEY=VALUE assignment per line. Values are never included in errors.
func ParseEnv(filePath string, data []byte) (map[string]string, error) {
	values := make(map[string]string)
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok {
			return nil, fmt.Errorf("env file %s line %d: expected KEY=VALUE", filePath, i+1)
		}
		if err := ValidateEnvName(name); err != nil {
			return nil, fmt.Errorf("env file %s line %d key %q: %w", filePath, i+1, name, err)
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("env file %s line %d: duplicate key %q", filePath, i+1, name)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		if strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("env file %s line %d key %q contains NUL", filePath, i+1, name)
		}
		values[name] = value
	}
	return values, nil
}

// ResolveEnvKeys expands selectors against available names. Selector order is
// preserved, glob matches are lexical, and duplicate concrete names collapse.
func ResolveEnvKeys(selectors []string, available map[string]string) ([]string, error) {
	names := make([]string, 0, len(available))
	for name := range available {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := make(map[string]struct{})
	resolved := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		matched := false
		for _, name := range names {
			ok := selector == name
			if strings.ContainsAny(selector, "*?[") {
				ok, _ = path.Match(selector, name)
			}
			if !ok {
				continue
			}
			matched = true
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			resolved = append(resolved, name)
		}
		if !matched {
			return nil, fmt.Errorf("env_keys entry %q does not resolve to any declared key", selector)
		}
	}
	return resolved, nil
}
