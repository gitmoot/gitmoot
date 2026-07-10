package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// MemoryIngestSource is one configured markdown source for the built-in
// memory-ingest-sweep pipeline (#783).
type MemoryIngestSource struct {
	Path  string
	Agent string
	Repo  string
	Tier  string
}

// MemoryPipelineSettings resolves the optional built-in memory pipeline config.
// Empty settings install the default pipelines as manual-only definitions.
type MemoryPipelineSettings struct {
	IngestSources        []MemoryIngestSource
	IngestSweepInterval  string
	IngestSweepJitter    string
	GroomProposeInterval string
	GroomProposeJitter   string
	Repo                 string
}

// LoadMemoryPipelineSettings reads:
//
//	[[memory.ingest]]
//	path = "/notes"
//	agent = "lead"
//	repo = "owner/repo"
//	tier = "repo"
//
//	[memory.pipelines]
//	ingest_sweep = "24h"
//	groom_propose = "24h"
//
// It is off by default: a config without those sections returns empty settings.
func LoadMemoryPipelineSettings(paths Paths) (MemoryPipelineSettings, error) {
	var settings MemoryPipelineSettings
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return MemoryPipelineSettings{}, err
	}

	current := ""
	var currentSource *MemoryIngestSource
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, array, ok := parseMemoryPipelineSection(line); ok {
			current = ""
			currentSource = nil
			switch section {
			case "memory.ingest":
				settings.IngestSources = append(settings.IngestSources, MemoryIngestSource{})
				currentSource = &settings.IngestSources[len(settings.IngestSources)-1]
				current = "memory.ingest"
				_ = array
			case "memory.pipelines":
				current = "memory.pipelines"
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch current {
		case "memory.ingest":
			if currentSource == nil {
				continue
			}
			if err := applyMemoryIngestSourceField(currentSource, key, value); err != nil {
				return MemoryPipelineSettings{}, fmt.Errorf("parse [[memory.ingest]].%s: %w", key, err)
			}
		case "memory.pipelines":
			if err := applyMemoryPipelineField(&settings, key, value); err != nil {
				return MemoryPipelineSettings{}, fmt.Errorf("parse [memory.pipelines].%s: %w", key, err)
			}
		}
	}
	if err := validateMemoryPipelineSettings(&settings); err != nil {
		return MemoryPipelineSettings{}, err
	}
	return settings, nil
}

func parseMemoryPipelineSection(line string) (section string, array bool, ok bool) {
	switch {
	case strings.HasPrefix(line, "[[") && strings.HasSuffix(line, "]]"):
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "[["), "]]")), true, true
	case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")), false, true
	default:
		return "", false, false
	}
}

func applyMemoryIngestSourceField(source *MemoryIngestSource, key string, value string) error {
	parsed, err := parseConfigString(value)
	if err != nil {
		return err
	}
	switch key {
	case "path":
		source.Path = parsed
	case "agent":
		source.Agent = parsed
	case "repo":
		source.Repo = parsed
	case "tier":
		source.Tier = parsed
	}
	return nil
}

func applyMemoryPipelineField(settings *MemoryPipelineSettings, key string, value string) error {
	switch key {
	case "repo":
		parsed, err := parseConfigString(value)
		if err != nil {
			return err
		}
		settings.Repo = parsed
	case "ingest_sweep", "ingest_sweep_interval":
		interval, err := parseMemoryPipelineInterval(value)
		if err != nil {
			return err
		}
		settings.IngestSweepInterval = interval
	case "ingest_sweep_jitter":
		jitter, err := parseMemoryPipelineJitter(value)
		if err != nil {
			return err
		}
		settings.IngestSweepJitter = jitter
	case "groom_propose", "groom_propose_interval":
		interval, err := parseMemoryPipelineInterval(value)
		if err != nil {
			return err
		}
		settings.GroomProposeInterval = interval
	case "groom_propose_jitter":
		jitter, err := parseMemoryPipelineJitter(value)
		if err != nil {
			return err
		}
		settings.GroomProposeJitter = jitter
	}
	return nil
}

func parseMemoryPipelineInterval(value string) (string, error) {
	parsed, err := parseConfigString(value)
	if err != nil {
		return "", err
	}
	parsed = strings.TrimSpace(parsed)
	switch strings.ToLower(parsed) {
	case "", "off", "disabled", "none":
		return "", nil
	case "nightly":
		return "24h", nil
	}
	d, err := time.ParseDuration(parsed)
	if err != nil {
		return "", fmt.Errorf("expected a Go duration like \"24h\" or \"nightly\": %w", err)
	}
	if d <= 0 {
		return "", fmt.Errorf("interval must be positive, got %q", parsed)
	}
	return parsed, nil
}

func parseMemoryPipelineJitter(value string) (string, error) {
	parsed, err := parseConfigString(value)
	if err != nil {
		return "", err
	}
	parsed = strings.TrimSpace(parsed)
	if parsed == "" {
		return "", nil
	}
	d, err := time.ParseDuration(parsed)
	if err != nil {
		return "", err
	}
	if d < 0 {
		return "", fmt.Errorf("jitter must be >= 0, got %q", parsed)
	}
	return parsed, nil
}

func validateMemoryPipelineSettings(settings *MemoryPipelineSettings) error {
	settings.Repo = strings.TrimSpace(settings.Repo)
	if settings.Repo != "" && !configRepoNameLooksValid(settings.Repo) {
		return fmt.Errorf("memory.pipelines.repo must be owner/repo, got %q", settings.Repo)
	}
	for i := range settings.IngestSources {
		source := &settings.IngestSources[i]
		source.Path = strings.TrimSpace(source.Path)
		source.Agent = strings.TrimSpace(source.Agent)
		source.Repo = strings.TrimSpace(source.Repo)
		source.Tier = strings.TrimSpace(source.Tier)
		if source.Tier == "" {
			source.Tier = "repo"
		}
		label := fmt.Sprintf("[[memory.ingest]] entry %d", i+1)
		if source.Path == "" {
			return fmt.Errorf("%s path is required", label)
		}
		if source.Agent == "" {
			return fmt.Errorf("%s agent is required", label)
		}
		if strings.ContainsAny(source.Agent, " \t\r\n") {
			return fmt.Errorf("%s agent %q must not contain whitespace", label, source.Agent)
		}
		switch source.Tier {
		case "repo":
			if source.Repo != "" && !configRepoNameLooksValid(source.Repo) {
				return fmt.Errorf("%s repo must be owner/repo, got %q", label, source.Repo)
			}
		case "general":
			if source.Repo != "" {
				return fmt.Errorf("%s tier general cannot set repo", label)
			}
		default:
			return fmt.Errorf("%s tier must be repo or general, got %q", label, source.Tier)
		}
	}
	return nil
}

func configRepoNameLooksValid(repo string) bool {
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	return ok && strings.TrimSpace(owner) != "" && strings.TrimSpace(name) != "" && !strings.Contains(name, "/")
}
