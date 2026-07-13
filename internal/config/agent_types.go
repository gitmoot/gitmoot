package config

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

type AgentType struct {
	Name           string
	Runtime        string
	Template       string
	Model          string
	Effort         string
	Role           string
	Capabilities   []string
	AutonomyPolicy string
	MaxBackground  int
	IdleTimeout    string
	JobTimeout     string
	// Memory enrolls this agent in persistent memory (#626). Default false
	// (off) — an agent that never sets it behaves byte-identically. Enrollment is
	// the per-agent switch for both the READ path (inject prior learnings) and the
	// Phase-1 SHADOW writes (log returned learnings to memory_observations).
	Memory bool
	// ChatAutoRespond enrolls this agent in the chat auto-respond sweep (#534
	// V1.5). Default false (off) — an agent that never sets it behaves
	// byte-identically. It is the per-agent opt-in that pairs with the global
	// [chat].auto_respond switch: BOTH must be true before the daemon sweep will
	// auto-enqueue a bounded read-only ask when this agent is @mentioned in a chat
	// message. Wired exactly like Memory above.
	ChatAutoRespond bool
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
				// GUARD (#533): a remainder containing a '.' is a SUBSECTION (e.g.
				// agents.x.heartbeats.y), not an agent. Without this it would register a
				// phantom agent named "x.heartbeats.y" and absorb the subsection's fields.
				// Clearing current both skips the registration and skips the subsection's
				// fields below; that subsection's own loader (e.g. LoadHeartbeats) owns it.
				// No existing (non-subsection) agent name contains a '.', so this never
				// changes behavior for any current config.
				if strings.Contains(current, ".") {
					current = ""
				}
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
	return saveAgentType(paths, entry, func(path string, content []byte) error {
		return os.WriteFile(path, content, 0o600)
	})
}

// SaveAgentTypeAtomic persists the complete agent-type registry through a
// same-directory temporary file and rename. Manual `agent start` uses this only
// after the runtime session and DB registration have succeeded, so a partial
// config write can never leave a truncated [agents.<name>] enrollment block.
func SaveAgentTypeAtomic(paths Paths, entry AgentType) error {
	return saveAgentType(paths, entry, writeConfigAtomic)
}

func saveAgentType(paths Paths, entry AgentType, write func(string, []byte) error) error {
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
	return write(paths.ConfigFile, []byte(builder.String()))
}

func applyAgentTypeDefaults(entry *AgentType) {
	entry.Name = strings.TrimSpace(entry.Name)
	entry.Runtime = strings.TrimSpace(entry.Runtime)
	entry.Template = strings.TrimSpace(entry.Template)
	entry.Model = strings.TrimSpace(entry.Model)
	entry.Effort = strings.TrimSpace(entry.Effort)
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
	case "model":
		parsed, err := parseConfigString(value)
		entry.Model = parsed
		return err
	case "effort":
		parsed, err := parseConfigString(value)
		entry.Effort = parsed
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
	case "memory":
		parsed, err := parseConfigBool(value)
		if err != nil {
			return err
		}
		entry.Memory = parsed
		return nil
	case "chat_autorespond":
		parsed, err := parseConfigBool(value)
		if err != nil {
			return err
		}
		entry.ChatAutoRespond = parsed
		return nil
	default:
		return nil
	}
}

// parseConfigBool parses a bare TOML boolean scalar (true|false), tolerating
// surrounding whitespace. Any other value is an error so a typo cannot silently
// leave an agent unenrolled (or enrolled).
func parseConfigBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("expected true or false, got %q", strings.TrimSpace(value))
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

// parseConfigFloat parses a bare TOML float scalar (e.g. 0.25) into a float64.
// It rejects NaN and the infinities so a malformed [skillopt] knob can never
// produce a sampling rate that bypasses the [0,1] bound check downstream. Unlike
// parseConfigString it does NOT unquote: a sampling rate is a number, not a
// quoted string. Callers (the [skillopt] loader) apply the range validation.
func parseConfigFloat(value string) (float64, error) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("not a finite number: %q", strings.TrimSpace(value))
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
			// Only strip TOP-LEVEL agent-type blocks. Mirror the LoadAgentTypes
			// guard (#533): a remainder containing a '.' is a SUBSECTION (e.g.
			// agents.x.heartbeats.y) owned by another loader. Stripping those here
			// would silently delete every heartbeat on an unrelated `agent type set`
			// rewrite, since SaveAgentType re-appends only the agent-TYPE blocks.
			rest := strings.TrimPrefix(section, "agents.")
			skip = strings.HasPrefix(section, "agents.") && !strings.Contains(rest, ".")
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
	if entry.Model != "" {
		builder.WriteString("model = ")
		builder.WriteString(strconv.Quote(entry.Model))
		builder.WriteString("\n")
	}
	if entry.Effort != "" {
		builder.WriteString("effort = ")
		builder.WriteString(strconv.Quote(entry.Effort))
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
	// Only emit memory when enrolled, so a config written for an agent that never
	// touched memory stays byte-identical to before this field existed.
	if entry.Memory {
		builder.WriteString("memory = true\n")
	}
	// Same discipline for chat auto-respond enrollment (#534 V1.5): only emit when
	// opted in, so an agent that never touched it round-trips byte-identically.
	if entry.ChatAutoRespond {
		builder.WriteString("chat_autorespond = true\n")
	}
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
