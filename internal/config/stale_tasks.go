package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	DefaultStaleTaskTTL          = 168 * time.Hour
	DefaultDelegationWorktreeTTL = 72 * time.Hour
)

// LoadPlannedTaskTTL resolves the hot-read [workflow].planned_ttl setting.
// Planned-task retirement is destructive to human planning context, so every
// absent, empty, zero, negative, or unparseable value disables it. Only an
// explicit positive Go duration opts a repository daemon into the sweep.
func LoadPlannedTaskTTL(paths Paths) (time.Duration, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
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
		if current != "workflow" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "planned_ttl" {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			value, err = parseConfigString(value)
			if err != nil {
				return 0, nil
			}
		}
		ttl, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || ttl <= 0 {
			return 0, nil
		}
		return ttl, nil
	}
	return 0, nil
}

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

// LoadDelegationWorktreeTTL resolves the hot-read
// [workflow].delegation_worktree_ttl setting. It is default-on at 72 hours:
// delegation worktrees are disposable once their owning job is final, and the
// grace period leaves ample time for post-mortem inspection while still bounding
// disk growth after a killed daemon misses the normal cleanup path. Omitted or
// empty uses the default, exactly "0" disables the TTL reclaim pass, and every
// other value must be a non-negative Go duration.
func LoadDelegationWorktreeTTL(paths Paths) (time.Duration, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultDelegationWorktreeTTL, nil
		}
		return 0, err
	}
	ttl := DefaultDelegationWorktreeTTL
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
		if !ok || strings.TrimSpace(key) != "delegation_worktree_ttl" {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			value, err = parseConfigString(value)
			if err != nil {
				return 0, fmt.Errorf("parse [workflow].delegation_worktree_ttl: %w", err)
			}
		}
		value = strings.TrimSpace(value)
		if value == "" {
			ttl = DefaultDelegationWorktreeTTL
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
			return 0, fmt.Errorf("invalid [workflow].delegation_worktree_ttl %q: %w", value, err)
		}
	}
	return ttl, nil
}
