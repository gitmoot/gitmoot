package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultTranscriptRetain        = 168 * time.Hour
	DefaultTranscriptMaxTotalBytes = int64(2 * 1024 * 1024 * 1024)
)

// TranscriptsConfig controls opt-in raw runtime transcript retention. Invalid
// or missing configuration always resolves to the safe disabled default.
type TranscriptsConfig struct {
	Enabled       bool
	Retain        time.Duration
	MaxTotalBytes int64
}

func DefaultTranscriptsConfig() TranscriptsConfig {
	return TranscriptsConfig{
		Enabled:       false,
		Retain:        DefaultTranscriptRetain,
		MaxTotalBytes: DefaultTranscriptMaxTotalBytes,
	}
}

// LoadTranscriptsConfig reads only [transcripts]. Capture is deliberately
// fail-closed: a missing file/section, malformed value, non-positive retention,
// or non-positive total cap returns the disabled defaults and never fails a job.
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
	if !found || !valid || cfg.Retain <= 0 || cfg.MaxTotalBytes <= 0 {
		return fallback
	}
	return cfg
}
