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

// TestSaveHeartbeatRuntimeOverride asserts a runtime override round-trips, and
// that OMITTING it (the default) writes NO runtime key — keeping a heartbeat
// without an override byte-identical to the pre-#611 shape.
func TestSaveHeartbeatRuntimeOverride(t *testing.T) {
	paths := newHeartbeatEditPaths(t, "")
	// With a runtime override: the key is written and round-trips.
	if err := SaveHeartbeat(paths, Heartbeat{
		Agent: "builder", Name: "nightly", Enabled: true, Repo: "o/r",
		Interval: "24h", Action: "implement", Runtime: "codex", Prompt: "tidy",
	}); err != nil {
		t.Fatalf("SaveHeartbeat with runtime: %v", err)
	}
	// Without a runtime override: the section must contain no `runtime =` line.
	if err := SaveHeartbeat(paths, Heartbeat{
		Agent: "planner", Name: "daily", Enabled: true, Repo: "o/r",
		Interval: "24h", Action: "ask", Prompt: "status",
	}); err != nil {
		t.Fatalf("SaveHeartbeat without runtime: %v", err)
	}
	raw, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "runtime = \"codex\"") {
		t.Fatalf("expected runtime override written, got:\n%s", text)
	}
	// The planner (no override) section must not carry a runtime key.
	plannerIdx := strings.Index(text, "[agents.planner.heartbeats.daily]")
	if plannerIdx < 0 {
		t.Fatalf("planner heartbeat section missing:\n%s", text)
	}
	if strings.Contains(text[plannerIdx:], "runtime =") {
		t.Fatalf("planner heartbeat (no override) must not write a runtime key:\n%s", text[plannerIdx:])
	}
	got, err := LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats: %v", err)
	}
	var builder, planner Heartbeat
	for _, hb := range got {
		switch hb.Agent {
		case "builder":
			builder = hb
		case "planner":
			planner = hb
		}
	}
	if builder.Runtime != "codex" || planner.Runtime != "" {
		t.Fatalf("runtime round-trip mismatch: builder=%q planner=%q", builder.Runtime, planner.Runtime)
	}
}

// TestSaveHeartbeatClearsRuntimeOnResaveWithoutOverride pins the #611 review LOW:
// re-saving an existing heartbeat WITHOUT a runtime must CLEAR a prior `runtime`
// key, not silently keep the stale override (the update path omits `runtime` from
// its field set when empty, so it is not overwritten — it must be removed).
func TestSaveHeartbeatClearsRuntimeOnResaveWithoutOverride(t *testing.T) {
	paths := newHeartbeatEditPaths(t, "")
	// First save WITH an override.
	if err := SaveHeartbeat(paths, Heartbeat{
		Agent: "builder", Name: "nightly", Enabled: true, Repo: "o/r",
		Interval: "24h", Action: "implement", Runtime: "codex", Prompt: "tidy",
	}); err != nil {
		t.Fatalf("SaveHeartbeat with runtime: %v", err)
	}
	// Re-save the SAME heartbeat WITHOUT a runtime: the override must be cleared.
	if err := SaveHeartbeat(paths, Heartbeat{
		Agent: "builder", Name: "nightly", Enabled: true, Repo: "o/r",
		Interval: "24h", Action: "implement", Prompt: "tidy",
	}); err != nil {
		t.Fatalf("SaveHeartbeat without runtime: %v", err)
	}
	raw, err := os.ReadFile(paths.ConfigFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(raw)
	section := strings.Index(text, "[agents.builder.heartbeats.nightly]")
	if section < 0 {
		t.Fatalf("saved heartbeat section missing:\n%s", text)
	}
	if strings.Contains(text[section:], "runtime =") {
		t.Fatalf("re-save without --runtime must clear the prior override, got:\n%s", string(raw))
	}
	got, err := LoadHeartbeats(paths)
	if err != nil {
		t.Fatalf("LoadHeartbeats: %v", err)
	}
	if len(got) != 1 || got[0].Runtime != "" {
		t.Fatalf("runtime override not cleared: %+v", got)
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
		Agent: "x", Name: "h", Repo: "o/r", Interval: "1h", Action: "deploy", Prompt: "p",
	})
	if err == nil {
		t.Fatalf("expected validation error for unsupported action")
	}
	content, _ := os.ReadFile(paths.ConfigFile)
	if strings.Contains(string(content), "heartbeats.h") {
		t.Fatalf("invalid heartbeat was written to disk:\n%s", string(content))
	}
}

// TestSaveHeartbeatRejectsBadRuntime asserts an unsupported runtime override never
// reaches disk (#611).
func TestSaveHeartbeatRejectsBadRuntime(t *testing.T) {
	paths := newHeartbeatEditPaths(t, "")
	err := SaveHeartbeat(paths, Heartbeat{
		Agent: "x", Name: "h", Repo: "o/r", Interval: "1h", Action: "ask", Runtime: "shell", Prompt: "p",
	})
	if err == nil {
		t.Fatalf("expected validation error for shell runtime override")
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
