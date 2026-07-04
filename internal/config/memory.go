package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Default read-path knobs for agent persistent memory (#626). The token budget
// and max-entries cap are the initial values from the RFC body; they are meant
// to be calibrated empirically by the Phase-1 measurement harness.
const (
	DefaultMemoryTokenBudget = 1500
	DefaultMemoryMaxEntries  = 15
)

// MemorySettings is the resolved, off-by-default global knob set for agent
// persistent memory, parsed from the optional [memory] section. Enrollment is
// per-agent ([agents.<name>] memory = true); this section only carries the
// shared read-path knobs plus a global kill switch. A config with no [memory]
// section resolves to the documented defaults, and — crucially — no agent is
// enrolled unless it opts in, so the whole feature is off and behavior is
// byte-identical.
type MemorySettings struct {
	// Disabled is the global kill switch. When true it overrides every per-agent
	// memory=true enrollment, disabling both the read and shadow-write paths.
	// Default false (absent section == not globally disabled), so enrollment alone
	// governs; an operator can flip this to turn the feature off box-wide without
	// editing every agent block.
	Disabled bool
	// TokenBudget caps the total estimated tokens of the injected learnings block.
	TokenBudget int
	// MaxEntries caps how many confirmed memories are considered for injection.
	MaxEntries int
}

// DefaultMemorySettings returns the off-by-default resolved settings.
func DefaultMemorySettings() MemorySettings {
	return MemorySettings{
		Disabled:    false,
		TokenBudget: DefaultMemoryTokenBudget,
		MaxEntries:  DefaultMemoryMaxEntries,
	}
}

// LoadMemorySettings resolves the [memory] section knobs. An absent section (or
// an absent key) yields the documented default for that knob. Out-of-range or
// malformed values are rejected so `gitmoot config set` surfaces the error.
func LoadMemorySettings(paths Paths) (MemorySettings, error) {
	settings := DefaultMemorySettings()
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return MemorySettings{}, err
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
		if current != "memory" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "disabled":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].disabled: %w", err)
			}
			settings.Disabled = parsed
		case "token_budget":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].token_budget: %w", err)
			}
			settings.TokenBudget = parsed
		case "max_entries":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return MemorySettings{}, fmt.Errorf("parse [memory].max_entries: %w", err)
			}
			settings.MaxEntries = parsed
		}
	}
	if err := validateMemorySettings(settings); err != nil {
		return MemorySettings{}, err
	}
	return settings, nil
}

func validateMemorySettings(s MemorySettings) error {
	if s.TokenBudget < 0 {
		return fmt.Errorf("memory.token_budget must be >= 0, got %d", s.TokenBudget)
	}
	if s.MaxEntries < 0 {
		return fmt.Errorf("memory.max_entries must be >= 0, got %d", s.MaxEntries)
	}
	return nil
}
