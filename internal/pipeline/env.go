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

// ParseEnvNames validates an env file and returns only its names. The right-hand
// side of each assignment is inspected only for structural validity and is never
// retained in the result.
func ParseEnvNames(filePath string, data []byte) (map[string]struct{}, error) {
	names := make(map[string]struct{})
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		equals := strings.IndexByte(line, '=')
		if equals < 0 {
			return nil, fmt.Errorf("env file %s line %d: expected KEY=VALUE", filePath, i+1)
		}
		name := strings.TrimSpace(line[:equals])
		if err := ValidateEnvName(name); err != nil {
			return nil, fmt.Errorf("env file %s line %d key %q: %w", filePath, i+1, name, err)
		}
		if _, duplicate := names[name]; duplicate {
			return nil, fmt.Errorf("env file %s line %d: duplicate key %q", filePath, i+1, name)
		}
		if strings.ContainsRune(line[equals+1:], '\x00') {
			return nil, fmt.Errorf("env file %s line %d key %q contains NUL", filePath, i+1, name)
		}
		names[name] = struct{}{}
	}
	return names, nil
}

// EnvKeySource is one value-free namespace available to env_keys selectors.
// Sources are supplied in precedence order, highest first.
type EnvKeySource struct {
	Source string
	Mode   string
	Names  map[string]struct{}
}

// EnvKeyProjection is one concrete name selected from an EnvKeySource.
type EnvKeyProjection struct {
	Name   string
	Source string
	Mode   string
}

// ProjectEnvKeys expands selectors over name sets without accepting or
// returning credential values. Selector order is preserved, glob matches are
// lexical, duplicate concrete names collapse, and unresolved selectors are
// returned in their original order.
func ProjectEnvKeys(selectors []string, sources []EnvKeySource) (resolved []EnvKeyProjection, unresolved []string) {
	type sourceMeta struct {
		source string
		mode   string
	}
	winners := make(map[string]sourceMeta)
	for _, candidate := range sources {
		for name := range candidate.Names {
			if _, exists := winners[name]; exists {
				continue
			}
			winners[name] = sourceMeta{source: candidate.Source, mode: candidate.Mode}
		}
	}
	names := make([]string, 0, len(winners))
	for name := range winners {
		names = append(names, name)
	}
	sort.Strings(names)

	seen := make(map[string]struct{})
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
			meta := winners[name]
			resolved = append(resolved, EnvKeyProjection{Name: name, Source: meta.source, Mode: meta.mode})
		}
		if !matched {
			unresolved = append(unresolved, selector)
		}
	}
	return resolved, unresolved
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
