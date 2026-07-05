package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/gitmoot/internal/runtime"
)

// Heartbeat is one named recurring agent schedule parsed from an
// [agents.<agent>.heartbeats.<name>] config section (#533). The daemon's
// heartbeat scan enqueues a normal background job for each enabled+due entry,
// reusing the standard job queue/background-agent path (no separate runner). The
// loader mirrors the AgentType idioms (Load/applyField/Defaults/validate).
type Heartbeat struct {
	Agent         string
	Name          string
	Enabled       bool
	Repo          string
	Interval      string
	Jitter        string
	Action        string
	Prompt        string
	MaxConcurrent int
	// Runtime, when non-empty, runs this heartbeat's job through the named runtime
	// instead of the target agent's registered default runtime (#611, reusing the
	// per-job override machinery from #531). It is OPTIONAL: an empty Runtime leaves
	// every existing heartbeat byte-identical and runs on the agent default. Only a
	// resumable runtime (codex/claude/kimi) is allowed — a heartbeat mints a fresh
	// session, and shell sessions are whole commands, so shell is rejected.
	Runtime string
}

// LoadHeartbeats collects every [agents.<agent>.heartbeats.<name>] section from
// the config file into a stable, validated slice (config order preserved). It is
// OFF BY DEFAULT: a config with no heartbeat subsections returns an empty slice
// and never errors, and a caller that finds an empty slice does no further work.
//
// It reuses the same line-scanner shape as LoadAgentTypes. The agent-types
// loader carries a guard (see LoadAgentTypes) so these subsection headers are NOT
// mis-registered as phantom agents named "<agent>.heartbeats.<name>"; this loader
// owns them instead.
func LoadHeartbeats(paths Paths) ([]Heartbeat, error) {
	content, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		return nil, err
	}
	type key struct{ agent, name string }
	collected := map[key]*Heartbeat{}
	order := make([]key, 0)
	var current *Heartbeat
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = nil
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			agent, name, ok := parseHeartbeatSection(section)
			if !ok {
				continue
			}
			k := key{agent: agent, name: name}
			if collected[k] == nil {
				collected[k] = &Heartbeat{Agent: agent, Name: name}
				order = append(order, k)
			}
			current = collected[k]
			continue
		}
		if current == nil {
			continue
		}
		field, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if err := applyHeartbeatField(current, strings.TrimSpace(field), strings.TrimSpace(value)); err != nil {
			return nil, fmt.Errorf("parse [agents.%s.heartbeats.%s].%s: %w", current.Agent, current.Name, strings.TrimSpace(field), err)
		}
	}
	heartbeats := make([]Heartbeat, 0, len(order))
	for _, k := range order {
		entry := collected[k]
		applyHeartbeatDefaults(entry)
		if err := validateHeartbeat(*entry); err != nil {
			return nil, err
		}
		heartbeats = append(heartbeats, *entry)
	}
	return heartbeats, nil
}

// HeartbeatActions lists the actions a heartbeat may schedule: the read-only
// "ask" (the conservative default) and "review", plus the WRITE action
// "implement". "implement" is structurally valid here but POLICY-GATED (#611):
// it only runs for a target agent whose autonomy policy grants headless writes
// (workspace-write / danger-full-access) and that holds the "implement"
// capability. That gate is agent-aware (it needs the agent registry), so it is
// enforced at the CLI write path and the daemon scan, mirroring how the "review"
// capability check lives outside this pure config loader.
func HeartbeatActions() []string { return []string{"ask", "review", "implement"} }

// HeartbeatActionSupported reports whether action is one a heartbeat may use.
func HeartbeatActionSupported(action string) bool {
	switch strings.TrimSpace(action) {
	case "ask", "review", "implement":
		return true
	default:
		return false
	}
}

// HeartbeatRuntimes lists the runtimes a per-heartbeat runtime override may name
// (#611): every resumable runtime the adapter Factory supports EXCEPT shell (a
// heartbeat mints a fresh session, and shell sessions are whole commands, not
// resumable sessions). It is derived from runtime.SupportedRuntimes so the set
// stays in lockstep with the actual adapter registry.
func HeartbeatRuntimes() []string {
	allowed := make([]string, 0, 4)
	for _, name := range runtime.SupportedRuntimes() {
		if name == runtime.ShellRuntime {
			continue
		}
		allowed = append(allowed, name)
	}
	return allowed
}

// HeartbeatRuntimeSupported reports whether rt is a valid per-heartbeat runtime
// override (a resumable runtime, never shell). The empty string is valid: it
// means "no override; run on the agent default".
func HeartbeatRuntimeSupported(rt string) bool {
	rt = strings.TrimSpace(rt)
	if rt == "" {
		return true
	}
	for _, name := range HeartbeatRuntimes() {
		if name == rt {
			return true
		}
	}
	return false
}

// parseHeartbeatSection extracts the agent and the heartbeat name from a section
// of the form agents.<agent>.heartbeats.<name>. It returns ok=false for any other
// section (including a deeper sub-subsection under <name>), so unrelated sections
// are ignored.
func parseHeartbeatSection(section string) (agent string, name string, ok bool) {
	section = strings.TrimSpace(section)
	if !strings.HasPrefix(section, "agents.") {
		return "", "", false
	}
	rest := strings.TrimPrefix(section, "agents.")
	const marker = ".heartbeats."
	index := strings.Index(rest, marker)
	if index < 0 {
		return "", "", false
	}
	agent = strings.TrimSpace(rest[:index])
	name = strings.TrimSpace(rest[index+len(marker):])
	if agent == "" || name == "" {
		return "", "", false
	}
	// The name must be a leaf: a further '.' would be a deeper subsection this MVP
	// does not define, so reject it rather than silently truncating.
	if strings.Contains(name, ".") {
		return "", "", false
	}
	return agent, name, true
}

func applyHeartbeatField(entry *Heartbeat, key string, value string) error {
	switch key {
	case "enabled":
		parsed, err := strconv.ParseBool(value)
		entry.Enabled = parsed
		return err
	case "repo":
		parsed, err := parseConfigString(value)
		entry.Repo = parsed
		return err
	case "interval":
		parsed, err := parseConfigString(value)
		entry.Interval = parsed
		return err
	case "jitter":
		parsed, err := parseConfigString(value)
		entry.Jitter = parsed
		return err
	case "action":
		parsed, err := parseConfigString(value)
		entry.Action = parsed
		return err
	case "prompt":
		parsed, err := parseConfigString(value)
		entry.Prompt = parsed
		return err
	case "runtime":
		parsed, err := parseConfigString(value)
		entry.Runtime = parsed
		return err
	case "max_concurrent":
		parsed, err := strconv.Atoi(value)
		entry.MaxConcurrent = parsed
		return err
	default:
		return nil
	}
}

func applyHeartbeatDefaults(entry *Heartbeat) {
	entry.Agent = strings.TrimSpace(entry.Agent)
	entry.Name = strings.TrimSpace(entry.Name)
	entry.Repo = strings.TrimSpace(entry.Repo)
	entry.Interval = strings.TrimSpace(entry.Interval)
	entry.Jitter = strings.TrimSpace(entry.Jitter)
	if entry.Jitter == "" {
		entry.Jitter = "0s"
	}
	entry.Action = strings.TrimSpace(entry.Action)
	if entry.Action == "" {
		entry.Action = "ask"
	}
	entry.Prompt = strings.TrimSpace(entry.Prompt)
	entry.Runtime = strings.TrimSpace(entry.Runtime)
	if entry.MaxConcurrent <= 0 {
		entry.MaxConcurrent = 1
	}
}

// validateHeartbeat enforces the MVP contract with explicit errors (matching the
// agent_types.go validation style). Durations are validated via
// time.ParseDuration so an invalid interval/jitter is a clear config error rather
// than a silent skip.
func validateHeartbeat(entry Heartbeat) error {
	if entry.Repo == "" {
		return fmt.Errorf("heartbeat [agents.%s.heartbeats.%s]: repo is required", entry.Agent, entry.Name)
	}
	if entry.Interval == "" {
		return fmt.Errorf("heartbeat [agents.%s.heartbeats.%s]: interval is required", entry.Agent, entry.Name)
	}
	if _, err := time.ParseDuration(entry.Interval); err != nil {
		return fmt.Errorf("heartbeat [agents.%s.heartbeats.%s]: interval %q: %w", entry.Agent, entry.Name, entry.Interval, err)
	}
	if _, err := time.ParseDuration(entry.Jitter); err != nil {
		return fmt.Errorf("heartbeat [agents.%s.heartbeats.%s]: jitter %q: %w", entry.Agent, entry.Name, entry.Jitter, err)
	}
	// Supported actions are the read-only "ask" (analyze/answer) and "review"
	// (read-only PR/code review), plus the WRITE action "implement" (#611). A
	// "review" heartbeat additionally requires the target agent to HOLD the review
	// capability, and an "implement" heartbeat requires the agent to hold the
	// implement capability AND carry a write-granting autonomy policy; both checks
	// need the agent registry, so they live in the daemon scan (runOneHeartbeat) and
	// the CLI write path, not in this pure config loader.
	if !HeartbeatActionSupported(entry.Action) {
		return fmt.Errorf("heartbeat [agents.%s.heartbeats.%s]: unsupported action %q; supported actions are %s", entry.Agent, entry.Name, entry.Action, strings.Join(HeartbeatActions(), ", "))
	}
	// A per-heartbeat runtime override (#611) must name a resumable runtime the
	// adapter Factory supports (codex/claude/kimi), never shell. An empty runtime is
	// valid and means "run on the agent default" (the byte-identical default).
	if !HeartbeatRuntimeSupported(entry.Runtime) {
		return fmt.Errorf("heartbeat [agents.%s.heartbeats.%s]: unsupported runtime %q; supported runtimes are %s", entry.Agent, entry.Name, entry.Runtime, strings.Join(HeartbeatRuntimes(), ", "))
	}
	if entry.Prompt == "" {
		return fmt.Errorf("heartbeat [agents.%s.heartbeats.%s]: prompt is required", entry.Agent, entry.Name)
	}
	return nil
}
