package workflow

import (
	"testing"

	"github.com/jerryfane/gitmoot/internal/db"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

// TestDecidePresetReferenceEveryModeStateCombination exercises the pure #33
// decision across every mode x state combination on a concrete, resumable
// session that carries a preset. Only referenced+state and auto+state+persisted
// runtime may shorten; everything else falls back to full (send the whole body).
func TestDecidePresetReferenceEveryModeStateCombination(t *testing.T) {
	concrete := "codex-thread-123"
	cases := []struct {
		name     string
		mode     string
		runtime  string
		hasState bool
		want     bool
	}{
		{"full+state", db.PresetDeliveryFull, runtime.CodexRuntime, true, false},
		{"full+nostate", db.PresetDeliveryFull, runtime.CodexRuntime, false, false},
		{"empty-defaults-full+state", "", runtime.CodexRuntime, true, false},
		{"referenced+state+codex", db.PresetDeliveryReferenced, runtime.CodexRuntime, true, true},
		{"referenced+nostate+codex", db.PresetDeliveryReferenced, runtime.CodexRuntime, false, false},
		{"referenced+state+shell", db.PresetDeliveryReferenced, runtime.ShellRuntime, true, true},
		{"referenced+nostate+shell", db.PresetDeliveryReferenced, runtime.ShellRuntime, false, false},
		{"auto+state+codex", db.PresetDeliveryAuto, runtime.CodexRuntime, true, true},
		{"auto+state+claude", db.PresetDeliveryAuto, runtime.ClaudeRuntime, true, true},
		{"auto+nostate+codex", db.PresetDeliveryAuto, runtime.CodexRuntime, false, false},
		{"auto+state+shell-unsupported", db.PresetDeliveryAuto, runtime.ShellRuntime, true, false},
		{"auto+state+kimi-unsupported", db.PresetDeliveryAuto, runtime.KimiRuntime, true, false},
		{"unknown-mode-defaults-full", "loose", runtime.CodexRuntime, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decidePresetReference(presetDeliveryInputs{
				Mode:       tc.mode,
				Runtime:    tc.runtime,
				SessionRef: concrete,
				HasPreset:  true,
				HasState:   tc.hasState,
			})
			if got != tc.want {
				t.Fatalf("decidePresetReference(%+v) = %v, want %v", tc, got, tc.want)
			}
		})
	}
}

// TestDecidePresetReferenceRequiresConcreteSession pins that an ambiguous or
// brand-new session always falls back to full even in referenced/auto with a
// state marker present: empty ref, "last", and a fresh: ref are all doubt.
func TestDecidePresetReferenceRequiresConcreteSession(t *testing.T) {
	for _, ref := range []string{"", runtime.LastRef, "fresh:codex-solo", runtime.FreshRefPrefix + "abc"} {
		for _, mode := range []string{db.PresetDeliveryReferenced, db.PresetDeliveryAuto} {
			if decidePresetReference(presetDeliveryInputs{
				Mode:       mode,
				Runtime:    runtime.CodexRuntime,
				SessionRef: ref,
				HasPreset:  true,
				HasState:   true,
			}) {
				t.Fatalf("mode=%s ref=%q unexpectedly shortened; a non-concrete session must send full", mode, ref)
			}
		}
	}
}

// TestDecidePresetReferenceRequiresPreset pins that with no preset to reference
// (missing id/content) the decision is always full, regardless of mode/state.
func TestDecidePresetReferenceRequiresPreset(t *testing.T) {
	if decidePresetReference(presetDeliveryInputs{
		Mode:       db.PresetDeliveryReferenced,
		Runtime:    runtime.CodexRuntime,
		SessionRef: "codex-thread-123",
		HasPreset:  false,
		HasState:   true,
	}) {
		t.Fatalf("no preset must never shorten")
	}
}

func TestRuntimeSupportsPersistedSessions(t *testing.T) {
	yes := []string{runtime.CodexRuntime, runtime.ClaudeRuntime}
	no := []string{runtime.ShellRuntime, runtime.KimiRuntime, runtime.KimiCLIRuntime, "custom", ""}
	for _, rt := range yes {
		if !runtimeSupportsPersistedSessions(rt) {
			t.Fatalf("runtime %q should support persisted sessions", rt)
		}
	}
	for _, rt := range no {
		if runtimeSupportsPersistedSessions(rt) {
			t.Fatalf("runtime %q should NOT support persisted sessions", rt)
		}
	}
}

func TestNormalizePresetDeliveryMode(t *testing.T) {
	cases := map[string]string{
		"":           db.PresetDeliveryFull,
		"  ":         db.PresetDeliveryFull,
		"FULL":       db.PresetDeliveryFull,
		"Referenced": db.PresetDeliveryReferenced,
		" auto ":     db.PresetDeliveryAuto,
		"nonsense":   db.PresetDeliveryFull,
	}
	for in, want := range cases {
		if got := normalizePresetDeliveryMode(in); got != want {
			t.Fatalf("normalizePresetDeliveryMode(%q) = %q, want %q", in, got, want)
		}
	}
}
