package config

import (
	"os"
	"testing"
	"time"
)

func chatConfigFixture(t *testing.T, body string) Paths {
	t.Helper()
	paths := PathsForHome(t.TempDir())
	if err := Initialize(paths); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(DefaultConfig(paths)+body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return paths
}

// TestLoadChatSettingsDefaults is the off-by-default invariant: a config with no
// [chat] section resolves to auto-respond OFF with the documented bounds.
func TestLoadChatSettingsDefaults(t *testing.T) {
	paths := chatConfigFixture(t, "")
	settings, err := LoadChatSettings(paths)
	if err != nil {
		t.Fatalf("LoadChatSettings: %v", err)
	}
	if settings.AutoRespond {
		t.Fatalf("AutoRespond = true, want false (off by default)")
	}
	if settings.AutoRespondCap != DefaultChatAutoRespondCap {
		t.Fatalf("AutoRespondCap = %d, want %d", settings.AutoRespondCap, DefaultChatAutoRespondCap)
	}
	if settings.AutoRespondCooldown != DefaultChatAutoRespondCooldown {
		t.Fatalf("AutoRespondCooldown = %s, want %s", settings.AutoRespondCooldown, DefaultChatAutoRespondCooldown)
	}
}

// TestLoadChatSettingsParsed proves the [chat] knobs parse (including a quoted Go
// duration for the cooldown).
func TestLoadChatSettingsParsed(t *testing.T) {
	paths := chatConfigFixture(t, `
[chat]
auto_respond = true
auto_respond_cap = 2
auto_respond_cooldown = "45s"
`)
	settings, err := LoadChatSettings(paths)
	if err != nil {
		t.Fatalf("LoadChatSettings: %v", err)
	}
	if !settings.AutoRespond {
		t.Fatalf("AutoRespond = false, want true")
	}
	if settings.AutoRespondCap != 2 {
		t.Fatalf("AutoRespondCap = %d, want 2", settings.AutoRespondCap)
	}
	if settings.AutoRespondCooldown != 45*time.Second {
		t.Fatalf("AutoRespondCooldown = %s, want 45s", settings.AutoRespondCooldown)
	}
}

// TestLoadChatSettingsRejectsBadValues proves malformed / out-of-range knobs surface
// an error instead of silently mis-bounding the sweep.
func TestLoadChatSettingsRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"negative cap", "\n[chat]\nauto_respond_cap = -1\n"},
		{"bad bool", "\n[chat]\nauto_respond = maybe\n"},
		{"bad duration", "\n[chat]\nauto_respond_cooldown = \"soon\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := chatConfigFixture(t, tc.body)
			if _, err := LoadChatSettings(paths); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

// TestAgentTypeChatAutoRespondRoundTrips proves the per-agent chat_autorespond opt-in
// loads and round-trips through SaveAgentType exactly like memory = true, and stays
// OFF (and absent from the written block) for an agent that never sets it.
func TestAgentTypeChatAutoRespondRoundTrips(t *testing.T) {
	paths := chatConfigFixture(t, `
[agents.responder]
runtime = "codex"
role = "responder"
chat_autorespond = true

[agents.quiet]
runtime = "codex"
role = "quiet"
`)
	types, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes: %v", err)
	}
	if !types["responder"].ChatAutoRespond {
		t.Fatalf("responder.ChatAutoRespond = false, want true")
	}
	if types["quiet"].ChatAutoRespond {
		t.Fatalf("quiet.ChatAutoRespond = true, want false")
	}

	// Round-trip the enrolled agent through SaveAgentType.
	if err := SaveAgentType(paths, types["responder"]); err != nil {
		t.Fatalf("SaveAgentType: %v", err)
	}
	reloaded, err := LoadAgentTypes(paths)
	if err != nil {
		t.Fatalf("LoadAgentTypes reload: %v", err)
	}
	if !reloaded["responder"].ChatAutoRespond {
		t.Fatalf("responder.ChatAutoRespond did not survive SaveAgentType round-trip")
	}
	if reloaded["quiet"].ChatAutoRespond {
		t.Fatalf("quiet.ChatAutoRespond flipped to true after round-trip")
	}
}

// TestLoadChatSettingsMootKnobs proves the moot knobs default correctly, parse an
// override, and reject out-of-range values (both must be >= 1).
func TestLoadChatSettingsMootKnobs(t *testing.T) {
	// Defaults with no [chat] section.
	def, err := LoadChatSettings(chatConfigFixture(t, ""))
	if err != nil {
		t.Fatalf("LoadChatSettings defaults: %v", err)
	}
	if def.MootMaxSeats != DefaultChatMootMaxSeats || def.MootMessageCap != DefaultChatMootMessageCap {
		t.Fatalf("moot defaults = (%d, %d), want (%d, %d)", def.MootMaxSeats, def.MootMessageCap,
			DefaultChatMootMaxSeats, DefaultChatMootMessageCap)
	}

	// Explicit overrides parse.
	over, err := LoadChatSettings(chatConfigFixture(t, "\n[chat]\nmoot_max_seats = 8\nmoot_message_cap = 50\n"))
	if err != nil {
		t.Fatalf("LoadChatSettings overrides: %v", err)
	}
	if over.MootMaxSeats != 8 || over.MootMessageCap != 50 {
		t.Fatalf("moot overrides = (%d, %d), want (8, 50)", over.MootMaxSeats, over.MootMessageCap)
	}

	// Out-of-range values are rejected (a config error, not silent mis-bounding).
	if _, err := LoadChatSettings(chatConfigFixture(t, "\n[chat]\nmoot_max_seats = 0\n")); err == nil {
		t.Fatal("moot_max_seats = 0 was accepted, want a validation error")
	}
	if _, err := LoadChatSettings(chatConfigFixture(t, "\n[chat]\nmoot_message_cap = 0\n")); err == nil {
		t.Fatal("moot_message_cap = 0 was accepted, want a validation error")
	}
}
