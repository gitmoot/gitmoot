package config

import (
	"os"
	"testing"
)

func TestLoadEventsPolicyDefaultsEmpty(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	policy, err := LoadEventsPolicy(paths)
	if err != nil {
		t.Fatalf("LoadEventsPolicy returned error: %v", err)
	}
	if policy != (EventsPolicy{}) {
		t.Fatalf("default EventsPolicy must be empty, got %+v", policy)
	}
}

// TestLoadEventsPolicyIgnoresRetiredWebhookKeys pins the #2583 cutover: a live
// config that still carries the removed webhook keys (even with values the old
// validator rejected) keeps loading, and only socket_path is read.
func TestLoadEventsPolicyIgnoresRetiredWebhookKeys(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[events]
webhook_url = "ftp://example.test/hook"
timeout = "not-a-duration"
socket_path = "/tmp/gitmoot-events.sock"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	policy, err := LoadEventsPolicy(paths)
	if err != nil {
		t.Fatalf("LoadEventsPolicy must ignore retired webhook keys, got error: %v", err)
	}
	if policy.SocketPath != "/tmp/gitmoot-events.sock" {
		t.Fatalf("SocketPath = %q, want /tmp/gitmoot-events.sock", policy.SocketPath)
	}
}

// TestLoadEventsPolicyDoesNotBleedIntoOrchestrate pins the hand-rolled scanner's
// section flag: an [events] section must not be parsed as [orchestrate] keys and
// vice-versa.
func TestLoadEventsPolicyDoesNotBleedIntoOrchestrate(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
escalation_handle = "@owner"
max_verify_replan_attempts = 4
socket_path = "/tmp/orchestrate.sock"

[events]
socket_path = "/tmp/events.sock"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	events, err := LoadEventsPolicy(paths)
	if err != nil {
		t.Fatalf("LoadEventsPolicy returned error: %v", err)
	}
	if events.SocketPath != "/tmp/events.sock" {
		t.Fatalf("events policy = %+v, want socket_path from [events] only", events)
	}

	orch, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if orch.MaxVerifyReplanAttempts != 4 {
		t.Fatalf("orchestrate policy = %+v, want verify cap=4 from [orchestrate] only", orch)
	}
}
