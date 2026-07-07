package config

import (
	"fmt"
	"os"
	"strings"
)

// RouterSettings is the resolved, off-by-default knob set for execution-grounded
// routing (#530), parsed from the optional [router] section. v1 is ADVISORY: the
// only behavioral knob is context_enabled, which decides whether a bounded
// observed-performance table is injected into a coordinator's prompt. Capture (the
// routing_telemetry rows) is always on and additive — it does not change any wire
// output — so it needs no knob. A config with no [router] section resolves to the
// defaults (context injection off), so behavior is byte-identical.
type RouterSettings struct {
	// ContextEnabled turns on the coordinator observed-performance context block.
	// Default false: with it off, coordinator prompt assembly is byte-identical and
	// no telemetry query runs during a job.
	ContextEnabled bool
}

// DefaultRouterSettings returns the off-by-default resolved settings.
func DefaultRouterSettings() RouterSettings {
	return RouterSettings{ContextEnabled: false}
}

// LoadRouterSettings resolves the [router] section knobs. An absent section (or an
// absent key) yields the documented default. A malformed value is rejected so
// `gitmoot config set` surfaces the error.
func LoadRouterSettings(paths Paths) (RouterSettings, error) {
	settings := DefaultRouterSettings()
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return RouterSettings{}, err
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
		if current != "router" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "context_enabled":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return RouterSettings{}, fmt.Errorf("parse [router].context_enabled: %w", err)
			}
			settings.ContextEnabled = parsed
		}
	}
	return settings, nil
}
