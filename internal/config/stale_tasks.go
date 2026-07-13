package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const DefaultStaleTaskTTL = 168 * time.Hour

// LoadStaleTaskTTL resolves the hot-read [workflow].stale_task_ttl setting.
// Omitted or empty uses seven days, exactly "0" disables reconciliation, and
// every other value must be a non-negative Go duration.
func LoadStaleTaskTTL(paths Paths) (time.Duration, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultStaleTaskTTL, nil
		}
		return 0, err
	}
	ttl := DefaultStaleTaskTTL
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
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "stale_task_ttl" {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			value, err = parseConfigString(value)
			if err != nil {
				return 0, fmt.Errorf("parse [workflow].stale_task_ttl: %w", err)
			}
		}
		value = strings.TrimSpace(value)
		if value == "" {
			ttl = DefaultStaleTaskTTL
			continue
		}
		if value == "0" {
			ttl = 0
			continue
		}
		ttl, err = time.ParseDuration(value)
		if err != nil || ttl < 0 {
			if err == nil {
				err = fmt.Errorf("duration must not be negative")
			}
			return 0, fmt.Errorf("invalid [workflow].stale_task_ttl %q: %w", value, err)
		}
	}
	return ttl, nil
}
