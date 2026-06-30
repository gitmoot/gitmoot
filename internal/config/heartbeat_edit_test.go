package config

import (
	"os"
	"strings"
	"testing"
)

func newHeartbeatEditPaths(t *testing.T, body string) Paths {
	t.Helper()
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	if body != "" {
		if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	return paths
}

func TestSaveHeartbeatCreateRoundTrip(t *testing.T) {
	paths := newHeartbeatEditPaths(t, "")
	entry := Heartbeat{
		Agent:    "repo-maintainer",
		Name:     "daily-status",
		Enabled:  true,
		Repo:     "jerryfane/gitmoot",
		Interval: "24h",
		Jitter:   "15m",
		Action:   "ask",
		Prompt:   "Review open issues and PRs.",
	}
	if err := SaveHeartbeat(paths, entry); err != nil {
		t.Fatalf("SaveHeartbeat: %v", err)
	}
	got, err := LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 heartbeat, got %d: %+v", len(got), got)
	}
	hb := got[0]
	if hb.Agent != "repo-maintainer" || hb.Name != "daily-status" || !hb.Enabled ||
		hb.Repo != "jerryfane/gitmoot" || hb.Interval != "24h" || hb.Jitter != "15m" ||
		hb.Action != "ask" || hb.Prompt != "Review open issues and PRs." || hb.MaxConcurrent != 1 {
		t.Fatalf("round-trip mismatch: %+v", hb)
	}
}

func TestSaveHeartbeatUpdateExisting(t *testing.T) {
	paths := newHeartbeatEditPaths(t, `
[agents.x.heartbeats.h]
enabled = false
repo = "o/r"
interval = "1h"
prompt = "old"
max_concurrent = 1
`)
	entry := Heartbeat{
		Agent:    "x",
		Name:     "h",
		Enabled:  true,
		Repo:     "o/r2",
		Interval: "2h",
		Action:   "ask",
		Prompt:   "new",
	}
	if err := SaveHeartbeat(paths, entry); err != nil {
		t.Fatalf("SaveHeartbeat update: %v", err)
	}
	got, err := LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 heartbeat after update, got %d", len(got))
	}
	hb := got[0]
	if !hb.Enabled || hb.Repo != "o/r2" || hb.Interval != "2h" || hb.Prompt != "new" {
		t.Fatalf("update did not apply: %+v", hb)
	}
}

// TestSaveHeartbeatPreservesAgentType is the no-clobber guard from the write side:
// writing a heartbeat must never drop an agent-type block.
func TestSaveHeartbeatPreservesAgentType(t *testing.T) {
	paths := newHeartbeatEditPaths(t, `
[agents.repo-maintainer]
runtime = "codex"
role = "repo-maintainer"
capabilities = ["ask", "review"]
max_background = 2
`)
	if err := SaveHeartbeat(paths, Heartbeat{
		Agent:    "repo-maintainer",
		Name:     "daily",
		Enabled:  true,
		Repo:     "o/r",
		Interval: "24h",
		Action:   "ask",
		Prompt:   "p",
	}); err != nil {
		t.Fatalf("SaveHeartbeat: %v", err)
	}
	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes: %v", err)
	}
	entry, ok := types["repo-maintainer"]
	if !ok {
		t.Fatalf("agent type clobbered by SaveHeartbeat: %v", keysOf(types))
	}
	if entry.Runtime != "codex" || entry.MaxBackground != 2 {
		t.Fatalf("agent type fields mangled: %+v", entry)
	}
	// And the reverse direction still holds: an agent-type edit keeps the heartbeat.
	entry.MaxBackground = 3
	if err := SaveAgentType(paths, entry); err != nil {
		t.Fatalf("SaveAgentType: %v", err)
	}
	heartbeats, err := LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats after SaveAgentType: %v", err)
	}
	if len(heartbeats) != 1 {
		t.Fatalf("heartbeat dropped by SaveAgentType: %d", len(heartbeats))
	}
}

// TestSaveHeartbeatPreservesSiblingHeartbeat asserts two heartbeats on the same
// agent are independent — writing one never touches the other.
func TestSaveHeartbeatPreservesSiblingHeartbeat(t *testing.T) {
	paths := newHeartbeatEditPaths(t, `
[agents.x.heartbeats.a]
enabled = true
repo = "o/r"
interval = "1h"
prompt = "alpha"

[agents.x.heartbeats.b]
enabled = false
repo = "o/r"
interval = "2h"
prompt = "beta"
`)
	if err := SaveHeartbeat(paths, Heartbeat{
		Agent: "x", Name: "a", Enabled: false, Repo: "o/r", Interval: "30m", Action: "ask", Prompt: "alpha2",
	}); err != nil {
		t.Fatalf("SaveHeartbeat: %v", err)
	}
	got, err := LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 heartbeats, got %d", len(got))
	}
	beta, ok := findTestHeartbeat(got, "x", "b")
	if !ok || beta.Prompt != "beta" || beta.Interval != "2h" || beta.Enabled {
		t.Fatalf("sibling heartbeat mangled: %+v", beta)
	}
}

func TestSetHeartbeatEnabledToggles(t *testing.T) {
	paths := newHeartbeatEditPaths(t, `
[agents.x.heartbeats.h]
enabled = false
repo = "o/r"
interval = "1h"
prompt = "p"
`)
	if err := SetHeartbeatEnabled(paths, "x", "h", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, _, err := loadHeartbeat(paths, "x", "h")
	if err != nil {
		t.Fatalf("loadHeartbeat: %v", err)
	}
	if !got.Enabled {
		t.Fatalf("expected enabled after enable")
	}
	if err := SetHeartbeatEnabled(paths, "x", "h", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _, err = loadHeartbeat(paths, "x", "h")
	if err != nil {
		t.Fatalf("loadHeartbeat: %v", err)
	}
	if got.Enabled {
		t.Fatalf("expected disabled after disable")
	}
}

// TestSetHeartbeatEnabledMissingEnabledKey covers a hand-written block that omits
// `enabled` (defaulting false): enable must still work via the upsert fallback.
func TestSetHeartbeatEnabledMissingEnabledKey(t *testing.T) {
	paths := newHeartbeatEditPaths(t, `
[agents.x.heartbeats.h]
repo = "o/r"
interval = "1h"
prompt = "p"
`)
	if err := SetHeartbeatEnabled(paths, "x", "h", true); err != nil {
		t.Fatalf("enable (missing key): %v", err)
	}
	got, ok, err := loadHeartbeat(paths, "x", "h")
	if err != nil || !ok {
		t.Fatalf("loadHeartbeat: ok=%v err=%v", ok, err)
	}
	if !got.Enabled {
		t.Fatalf("expected enabled after upsert fallback")
	}
}

func TestSetHeartbeatEnabledNotFound(t *testing.T) {
	paths := newHeartbeatEditPaths(t, "")
	if err := SetHeartbeatEnabled(paths, "x", "missing", true); err == nil {
		t.Fatalf("expected error enabling a non-existent heartbeat")
	}
}

func TestRemoveHeartbeat(t *testing.T) {
	paths := newHeartbeatEditPaths(t, `
[agents.x.heartbeats.h]
enabled = true
repo = "o/r"
interval = "1h"
prompt = "p"
`)
	removed, err := RemoveHeartbeat(paths, "x", "h")
	if err != nil {
		t.Fatalf("RemoveHeartbeat: %v", err)
	}
	if !removed {
		t.Fatalf("expected removed=true")
	}
	got, err := LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 heartbeats after remove, got %d", len(got))
	}
	// Removing again is a no-op (removed=false), not an error.
	removed, err = RemoveHeartbeat(paths, "x", "h")
	if err != nil {
		t.Fatalf("RemoveHeartbeat second: %v", err)
	}
	if removed {
		t.Fatalf("expected removed=false on second remove")
	}
}

// TestSaveHeartbeatRejectsInvalid asserts a bad action never reaches disk.
func TestSaveHeartbeatRejectsInvalid(t *testing.T) {
	paths := newHeartbeatEditPaths(t, "")
	err := SaveHeartbeat(paths, Heartbeat{
		Agent: "x", Name: "h", Repo: "o/r", Interval: "1h", Action: "implement", Prompt: "p",
	})
	if err == nil {
		t.Fatalf("expected validation error for implement action")
	}
	content, _ := os.ReadFile(paths.ConfigFile)
	if strings.Contains(string(content), "heartbeats.h") {
		t.Fatalf("invalid heartbeat was written to disk:\n%s", string(content))
	}
}

func findTestHeartbeat(heartbeats []Heartbeat, agent, name string) (Heartbeat, bool) {
	for _, hb := range heartbeats {
		if hb.Agent == agent && hb.Name == name {
			return hb, true
		}
	}
	return Heartbeat{}, false
}
