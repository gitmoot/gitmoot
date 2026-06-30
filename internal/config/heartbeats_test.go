package config

import (
	"os"
	"strings"
	"testing"
)

func writeHeartbeatConfig(t *testing.T, body string) Paths {
	t.Helper()
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return paths
}

func TestLoadHeartbeatsParsesAndDefaults(t *testing.T) {
	paths := writeHeartbeatConfig(t, `
[agents.repo-maintainer.heartbeats.daily-status]
enabled = true
repo = "jerryfane/gitmoot"
interval = "24h"
jitter = "15m"
action = "ask"
prompt = "Review open issues and PRs."
max_concurrent = 1

[agents.repo-maintainer.heartbeats.minimal]
enabled = false
repo = "jerryfane/gitmoot"
interval = "1h"
prompt = "Quick check."
`)
	heartbeats, err := LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats returned error: %v", err)
	}
	if len(heartbeats) != 2 {
		t.Fatalf("expected 2 heartbeats, got %d: %+v", len(heartbeats), heartbeats)
	}
	first := heartbeats[0]
	if first.Agent != "repo-maintainer" || first.Name != "daily-status" || !first.Enabled ||
		first.Repo != "jerryfane/gitmoot" || first.Interval != "24h" || first.Jitter != "15m" ||
		first.Action != "ask" || first.Prompt != "Review open issues and PRs." || first.MaxConcurrent != 1 {
		t.Fatalf("first heartbeat = %+v", first)
	}
	// Defaults: jitter -> 0s, action -> ask, max_concurrent -> 1.
	second := heartbeats[1]
	if second.Name != "minimal" || second.Enabled || second.Jitter != "0s" || second.Action != "ask" || second.MaxConcurrent != 1 {
		t.Fatalf("second heartbeat defaults = %+v", second)
	}
}

func TestLoadHeartbeatsOffByDefault(t *testing.T) {
	// A config with NO heartbeat sections must return an empty slice and no error,
	// so the daemon scan does no work (the off-by-default invariant).
	paths := writeHeartbeatConfig(t, `
[agents.planner]
runtime = "codex"
role = "planner"
`)
	heartbeats, err := LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats returned error: %v", err)
	}
	if len(heartbeats) != 0 {
		t.Fatalf("expected no heartbeats, got %+v", heartbeats)
	}
}

func TestLoadHeartbeatsValidationErrors(t *testing.T) {
	cases := map[string]string{
		"bad interval": `
[agents.x.heartbeats.h]
enabled = true
repo = "o/r"
interval = "not-a-duration"
prompt = "p"
`,
		"missing repo": `
[agents.x.heartbeats.h]
enabled = true
interval = "1h"
prompt = "p"
`,
		"missing prompt": `
[agents.x.heartbeats.h]
enabled = true
repo = "o/r"
interval = "1h"
`,
		"unsupported action": `
[agents.x.heartbeats.h]
enabled = true
repo = "o/r"
interval = "1h"
action = "implement"
prompt = "p"
`,
		"bad jitter": `
[agents.x.heartbeats.h]
enabled = true
repo = "o/r"
interval = "1h"
jitter = "nope"
prompt = "p"
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			paths := writeHeartbeatConfig(t, body)
			if _, err := LoadHeartbeats(paths); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
}

// TestLoadHeartbeatsAcceptsReviewAction asserts the review action now loads
// (the ask/review pair), while implement stays rejected (covered above).
func TestLoadHeartbeatsAcceptsReviewAction(t *testing.T) {
	paths := writeHeartbeatConfig(t, `
[agents.reviewer.heartbeats.stale-prs]
enabled = true
repo = "o/r"
interval = "12h"
action = "review"
prompt = "Review stale PRs."
`)
	heartbeats, err := LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats returned error: %v", err)
	}
	if len(heartbeats) != 1 || heartbeats[0].Action != "review" {
		t.Fatalf("expected one review heartbeat, got %+v", heartbeats)
	}
}

// TestLoadAgentTypesGuardIgnoresHeartbeatSubsections is the critical parser guard
// (#533): the agent-types line scanner must NOT register a phantom agent named
// "x.heartbeats.h" when it encounters a heartbeat subsection header.
func TestLoadAgentTypesGuardIgnoresHeartbeatSubsections(t *testing.T) {
	paths := writeHeartbeatConfig(t, `
[agents.x]
runtime = "codex"
role = "x"

[agents.x.heartbeats.h]
enabled = true
repo = "o/r"
interval = "1h"
prompt = "p"
`)
	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes returned error: %v", err)
	}
	if _, ok := types["x"]; !ok {
		t.Fatalf("expected real agent x to be registered, got %v", keysOf(types))
	}
	for name := range types {
		if strings.Contains(name, ".heartbeats.") || strings.Contains(name, ".") {
			t.Fatalf("phantom agent registered for subsection: %q (all: %v)", name, keysOf(types))
		}
	}
}

func keysOf(m map[string]AgentType) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	return names
}
