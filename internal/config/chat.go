package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Default knobs for the off-by-default chat auto-respond sweep (#534 V1.5). The
// cap and cooldown are the owner-decided design-jam values; both are calibratable
// via the [chat] section.
const (
	DefaultChatAutoRespondCap      = 4
	DefaultChatAutoRespondCooldown = 2 * time.Minute
)

// ChatSettings is the resolved, off-by-default global knob set for the chat
// auto-respond sweep, parsed from the optional [chat] section. Enrollment is
// PER-AGENT ([agents.<name>] chat_autorespond = true) — mirroring how [memory]
// pairs a global kill switch with per-agent memory=true opt-in. This section only
// carries the shared bounds plus the global enable switch. A config with no [chat]
// section resolves to AutoRespond=false, so the whole feature is inert and the
// daemon tick is byte-identical: the sweep returns before touching the chat tables.
type ChatSettings struct {
	// AutoRespond is the global kill switch (default false = OFF). The sweep only
	// runs when this is true; false overrides every per-agent chat_autorespond
	// opt-in, so an operator can turn the feature off box-wide without editing each
	// agent block.
	AutoRespond bool
	// AutoRespondCap is the HARD cap on auto-responses per (thread, agent). Once an
	// agent has produced this many auto-respond replies in a thread, the sweep stops
	// and parks the thread as needs-human (a visible system message) instead of
	// auto-extending. Must be >= 0.
	AutoRespondCap int
	// AutoRespondCooldown is the minimum spacing between auto-responses for the same
	// (thread, agent). A trigger seen inside the cooldown window is skipped (and left
	// unread so it re-fires after the window), never dropped. Must be >= 0.
	AutoRespondCooldown time.Duration
}

// DefaultChatSettings returns the off-by-default resolved settings.
func DefaultChatSettings() ChatSettings {
	return ChatSettings{
		AutoRespond:         false,
		AutoRespondCap:      DefaultChatAutoRespondCap,
		AutoRespondCooldown: DefaultChatAutoRespondCooldown,
	}
}

// LoadChatSettings resolves the [chat] section knobs. An absent section (or an
// absent key) yields the documented default for that knob. Out-of-range or
// malformed values are rejected so a bad config surfaces the error rather than
// silently mis-bounding the sweep.
func LoadChatSettings(paths Paths) (ChatSettings, error) {
	settings := DefaultChatSettings()
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return ChatSettings{}, err
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
		if current != "chat" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "auto_respond":
			parsed, err := parseConfigBool(value)
			if err != nil {
				return ChatSettings{}, fmt.Errorf("parse [chat].auto_respond: %w", err)
			}
			settings.AutoRespond = parsed
		case "auto_respond_cap":
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return ChatSettings{}, fmt.Errorf("parse [chat].auto_respond_cap: %w", err)
			}
			settings.AutoRespondCap = parsed
		case "auto_respond_cooldown":
			parsed, err := parseConfigDuration(value)
			if err != nil {
				return ChatSettings{}, fmt.Errorf("parse [chat].auto_respond_cooldown: %w", err)
			}
			settings.AutoRespondCooldown = parsed
		}
	}
	if err := validateChatSettings(settings); err != nil {
		return ChatSettings{}, err
	}
	return settings, nil
}

func validateChatSettings(s ChatSettings) error {
	if s.AutoRespondCap < 0 {
		return fmt.Errorf("chat.auto_respond_cap must be >= 0, got %d", s.AutoRespondCap)
	}
	if s.AutoRespondCooldown < 0 {
		return fmt.Errorf("chat.auto_respond_cooldown must be >= 0, got %s", s.AutoRespondCooldown)
	}
	return nil
}
