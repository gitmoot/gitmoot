package config

import (
	"os"
	"strings"
)

func LoadDefaultFeedbackRepo(paths Paths) (string, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return "", err
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
		if current != "feedback" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "repo" {
			continue
		}
		parsed, err := parseConfigString(strings.TrimSpace(value))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(parsed), nil
	}
	return "", nil
}
