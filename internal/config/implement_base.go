package config

import (
	"fmt"
	"os"
	"strings"
)

// LoadImplementBase resolves the optional [workflow].implement_base ref. An
// absent config file, section, key, or an explicitly empty string leaves the
// value unset so implement dispatch keeps its checkout-HEAD default.
func LoadImplementBase(paths Paths) (string, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	value := ""
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
		if current != "workflow" {
			continue
		}
		key, rawValue, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "implement_base" {
			continue
		}
		parsed, err := parseConfigString(strings.TrimSpace(rawValue))
		if err != nil {
			return "", fmt.Errorf("parse [workflow].implement_base: %w", err)
		}
		value = strings.TrimSpace(parsed)
		if err := validateImplementBase(value); err != nil {
			return "", err
		}
	}
	return value, nil
}

func validateImplementBase(value string) error {
	switch {
	case value == "":
		return nil
	case strings.HasPrefix(value, "-"):
		return fmt.Errorf("invalid [workflow].implement_base %q: ref must not start with '-'", value)
	case strings.ContainsAny(value, " \t\r\n"):
		return fmt.Errorf("invalid [workflow].implement_base %q: ref must not contain whitespace", value)
	default:
		return nil
	}
}
