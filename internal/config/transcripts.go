package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	MinimumTranscriptRetain        = 24 * time.Hour
	DefaultTranscriptRetain        = 168 * time.Hour
	DefaultTranscriptMaxTotalBytes = int64(2 * 1024 * 1024 * 1024)
)

// TranscriptsConfig controls default-on raw runtime transcript retention.
// Invalid or missing configuration resolves to safe bounded defaults.
type TranscriptsConfig struct {
	Enabled       bool
	Retain        time.Duration
	MaxTotalBytes int64
}

func DefaultTranscriptsConfig() TranscriptsConfig {
	return TranscriptsConfig{
		Enabled:       true,
		Retain:        DefaultTranscriptRetain,
		MaxTotalBytes: DefaultTranscriptMaxTotalBytes,
	}
}

// LoadTranscriptsConfig reads only [transcripts]. Capture is default-on so every
// engine delivery has a durable liveness/forensics stream. A malformed section
// falls back to the safe defaults; retention can never be configured below 24h.
func LoadTranscriptsConfig(paths Paths) TranscriptsConfig {
	fallback := DefaultTranscriptsConfig()
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return fallback
	}
	cfg := fallback
	found := false
	valid := true
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			current = section == "transcripts"
			found = found || current
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			valid = false
			continue
		}
		switch strings.TrimSpace(key) {
		case "enabled":
			parsed, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				valid = false
			} else {
				cfg.Enabled = parsed
			}
		case "retain":
			parsed, err := parseConfigString(strings.TrimSpace(value))
			if err != nil {
				valid = false
				continue
			}
			cfg.Retain, err = time.ParseDuration(strings.TrimSpace(parsed))
			if err != nil {
				valid = false
			}
		case "max_total_bytes":
			parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				valid = false
			} else {
				cfg.MaxTotalBytes = parsed
			}
		}
	}
	if !found {
		return fallback
	}
	if !valid || cfg.Retain < MinimumTranscriptRetain || cfg.MaxTotalBytes <= 0 {
		return fallback
	}
	return cfg
}
