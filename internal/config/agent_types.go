package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

type AgentType struct {
	Name           string
	Runtime        string
	Template       string
	Role           string
	Capabilities   []string
	AutonomyPolicy string
	MaxBackground  int
	IdleTimeout    string
	JobTimeout     string
}

func LoadAgentTypes(paths Paths) (map[string]AgentType, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return nil, err
	}
	types := map[string]AgentType{}
	var current string
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			current = ""
			if strings.HasPrefix(section, "agents.") {
				current = strings.TrimSpace(strings.TrimPrefix(section, "agents."))
				if current != "" {
					entry := types[current]
					entry.Name = current
					types[current] = entry
				}
			}
			continue
		}
		if current == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		entry := types[current]
		if err := applyAgentTypeField(&entry, strings.TrimSpace(key), strings.TrimSpace(value)); err != nil {
			return nil, fmt.Errorf("parse [agents.%s].%s: %w", current, strings.TrimSpace(key), err)
		}
		types[current] = entry
	}
	for name, entry := range types {
		entry.Name = name
		applyAgentTypeDefaults(&entry)
		types[name] = entry
	}
	return types, nil
}

func SaveAgentType(paths Paths, entry AgentType) error {
	types, err := LoadAgentTypes(paths)
	if err != nil {
		return err
	}
	applyAgentTypeDefaults(&entry)
	types[entry.Name] = entry
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return err
	}
	base := removeAgentTypeBlocks(string(content))
	var builder strings.Builder
	builder.WriteString(strings.TrimRight(base, "\n"))
	builder.WriteString("\n")
	names := make([]string, 0, len(types))
	for name := range types {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writeAgentTypeBlock(&builder, types[name])
	}
	return os.WriteFile(paths.ConfigFile, []byte(builder.String()), 0o600)
}

func applyAgentTypeDefaults(entry *AgentType) {
	entry.Name = strings.TrimSpace(entry.Name)
	entry.Runtime = strings.TrimSpace(entry.Runtime)
	entry.Template = strings.TrimSpace(entry.Template)
	entry.Role = strings.TrimSpace(entry.Role)
	if entry.Role == "" {
		entry.Role = entry.Name
	}
	entry.Capabilities = compactConfigStrings(entry.Capabilities)
	if len(entry.Capabilities) == 0 {
		entry.Capabilities = []string{"ask"}
	}
	if strings.TrimSpace(entry.AutonomyPolicy) == "" {
		entry.AutonomyPolicy = "auto"
	}
	if entry.MaxBackground <= 0 {
		entry.MaxBackground = 1
	}
	if strings.TrimSpace(entry.IdleTimeout) == "" {
		entry.IdleTimeout = "20m"
	}
	if strings.TrimSpace(entry.JobTimeout) == "" {
		entry.JobTimeout = "10m"
	}
}

func applyAgentTypeField(entry *AgentType, key string, value string) error {
	switch key {
	case "runtime":
		parsed, err := parseConfigString(value)
		entry.Runtime = parsed
		return err
	case "template":
		parsed, err := parseConfigString(value)
		entry.Template = parsed
		return err
	case "role":
		parsed, err := parseConfigString(value)
		entry.Role = parsed
		return err
	case "capabilities":
		parsed, err := parseConfigStringArray(value)
		entry.Capabilities = parsed
		return err
	case "autonomy_policy":
		parsed, err := parseConfigString(value)
		if err != nil {
			return err
		}
		parsed = strings.TrimSpace(parsed)
		if err := validateAgentTypeAutonomyPolicy(parsed); err != nil {
			return err
		}
		entry.AutonomyPolicy = parsed
		return nil
	case "max_background":
		parsed, err := strconv.Atoi(value)
		entry.MaxBackground = parsed
		return err
	case "idle_timeout":
		parsed, err := parseConfigString(value)
		entry.IdleTimeout = parsed
		return err
	case "job_timeout":
		parsed, err := parseConfigString(value)
		entry.JobTimeout = parsed
		return err
	default:
		return nil
	}
}

func validateAgentTypeAutonomyPolicy(policy string) error {
	switch strings.TrimSpace(policy) {
	case "", "auto", "read-only", "workspace-write", "danger-full-access":
		return nil
	default:
		return fmt.Errorf("unsupported autonomy policy %q; use auto, read-only, workspace-write, or danger-full-access", strings.TrimSpace(policy))
	}
}

func stripConfigComment(value string) string {
	inString := false
	escaped := false
	for index, r := range value {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && inString:
			escaped = true
		case r == '"':
			inString = !inString
		case r == '#' && !inString:
			return value[:index]
		}
	}
	return value
}

func parseConfigString(value string) (string, error) {
	parsed, err := strconv.Unquote(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	return parsed, nil
}

func parseConfigStringArray(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, fmt.Errorf("expected string array")
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"))
	if inner == "" {
		return nil, nil
	}
	parts := strings.Split(inner, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		parsed, err := parseConfigString(part)
		if err != nil {
			return nil, err
		}
		values = append(values, parsed)
	}
	return compactConfigStrings(values), nil
}

func removeAgentTypeBlocks(content string) string {
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	skip := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")
			skip = strings.HasPrefix(section, "agents.")
		}
		if !skip {
			kept = append(kept, line)
		}
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n"
}

func writeAgentTypeBlock(builder *strings.Builder, entry AgentType) {
	builder.WriteString("\n[agents.")
	builder.WriteString(entry.Name)
	builder.WriteString("]\n")
	builder.WriteString("runtime = ")
	builder.WriteString(strconv.Quote(entry.Runtime))
	builder.WriteString("\n")
	if entry.Template != "" {
		builder.WriteString("template = ")
		builder.WriteString(strconv.Quote(entry.Template))
		builder.WriteString("\n")
	}
	builder.WriteString("role = ")
	builder.WriteString(strconv.Quote(entry.Role))
	builder.WriteString("\ncapabilities = [")
	for index, capability := range entry.Capabilities {
		if index > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(strconv.Quote(capability))
	}
	builder.WriteString("]\nautonomy_policy = ")
	builder.WriteString(strconv.Quote(entry.AutonomyPolicy))
	builder.WriteString("\nmax_background = ")
	builder.WriteString(strconv.Itoa(entry.MaxBackground))
	builder.WriteString("\nidle_timeout = ")
	builder.WriteString(strconv.Quote(entry.IdleTimeout))
	builder.WriteString("\njob_timeout = ")
	builder.WriteString(strconv.Quote(entry.JobTimeout))
	builder.WriteString("\n")
}

func compactConfigStrings(values []string) []string {
	compacted := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			compacted = append(compacted, value)
		}
	}
	return compacted
}
