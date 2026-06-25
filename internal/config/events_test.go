package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadEventsPolicyDefaultsDisabled(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}

	policy, err := LoadEventsPolicy(paths)
	if err != nil {
		t.Fatalf("LoadEventsPolicy returned error: %v", err)
	}
	// With no [events] section the stream is OFF: no sink should be constructed.
	if policy.Enabled() {
		t.Fatalf("default EventsPolicy must be disabled, got %+v", policy)
	}
	if policy.WebhookURL != "" || policy.Timeout != "" || policy.SocketPath != "" {
		t.Fatalf("default EventsPolicy must be all-empty, got %+v", policy)
	}
	// ResolvedTimeout falls back to the documented default.
	if got := policy.ResolvedTimeout(); got != 2*time.Second {
		t.Fatalf("ResolvedTimeout default = %v, want 2s", got)
	}
}

func TestLoadEventsPolicyParsesWebhookAndTimeout(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[events]
webhook_url = "https://example.test/hook"
timeout = "5s"
socket_path = "/tmp/events.sock"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	policy, err := LoadEventsPolicy(paths)
	if err != nil {
		t.Fatalf("LoadEventsPolicy returned error: %v", err)
	}
	if !policy.Enabled() {
		t.Fatalf("EventsPolicy with webhook_url should be enabled, got %+v", policy)
	}
	if policy.WebhookURL != "https://example.test/hook" {
		t.Fatalf("WebhookURL = %q", policy.WebhookURL)
	}
	if policy.Timeout != "5s" {
		t.Fatalf("Timeout = %q, want 5s", policy.Timeout)
	}
	if got := policy.ResolvedTimeout(); got != 5*time.Second {
		t.Fatalf("ResolvedTimeout = %v, want 5s", got)
	}
	if policy.SocketPath != "/tmp/events.sock" {
		t.Fatalf("SocketPath (reserved) = %q, want /tmp/events.sock", policy.SocketPath)
	}
}

func TestLoadEventsPolicyRejectsInvalidTimeout(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[events]
webhook_url = "https://example.test/hook"
timeout = "not-a-duration"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if _, err := LoadEventsPolicy(paths); err == nil {
		t.Fatal("LoadEventsPolicy must reject an invalid timeout duration")
	}
}

func TestLoadEventsPolicyRejectsNonHTTPURL(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[events]
webhook_url = "ftp://example.test/hook"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}
	if _, err := LoadEventsPolicy(paths); err == nil {
		t.Fatal("LoadEventsPolicy must reject a non-http(s) webhook_url")
	}
}

// TestLoadEventsPolicyDoesNotBleedIntoOrchestrate pins the hand-rolled scanner's
// section flag: an [events] section must not be parsed as [orchestrate] keys and
// vice-versa. Interleave the two sections and assert each loader reads only its
// own keys.
func TestLoadEventsPolicyDoesNotBleedIntoOrchestrate(t *testing.T) {
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+`
[orchestrate]
cockpit_mode = "on"
max_verify_replan_attempts = 4

[events]
webhook_url = "http://127.0.0.1:9999/"
timeout = "3s"
`), 0o600); err != nil {
		t.Fatalf("write config returned error: %v", err)
	}

	events, err := LoadEventsPolicy(paths)
	if err != nil {
		t.Fatalf("LoadEventsPolicy returned error: %v", err)
	}
	if events.WebhookURL != "http://127.0.0.1:9999/" || events.Timeout != "3s" {
		t.Fatalf("events policy = %+v, want webhook+timeout from [events] only", events)
	}

	orch, err := LoadOrchestratePolicy(paths)
	if err != nil {
		t.Fatalf("LoadOrchestratePolicy returned error: %v", err)
	}
	if orch.CockpitMode != CockpitModeOn || orch.MaxVerifyReplanAttempts != 4 {
		t.Fatalf("orchestrate policy = %+v, want cockpit_mode=on + verify cap=4 from [orchestrate] only", orch)
	}
}
