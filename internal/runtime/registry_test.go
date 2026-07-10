package runtime

import (
	"reflect"
	"strings"
	"testing"
)

// TestBuiltinRegistryMatchesSupportedRuntimes proves the built-in metadata
// registry's dispatchable entries are exactly the runtimes the Factory can
// construct, in the same order — so the registry, SupportedRuntimes, and the
// adapter Factory share one source of truth.
func TestBuiltinRegistryMatchesSupportedRuntimes(t *testing.T) {
	reg := BuiltinRuntimeRegistry()
	got := reg.dispatchableNames()
	want := []string{CodexRuntime, ClaudeRuntime, KimiRuntime, KimiCLIRuntime, ShellRuntime}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatchableNames() = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(SupportedRuntimes(), want) {
		t.Fatalf("SupportedRuntimes() = %v, want %v", SupportedRuntimes(), want)
	}
	for _, name := range got {
		adapter, err := (Factory{}).Adapter(name)
		if err != nil {
			t.Fatalf("Factory.Adapter(%q) error: %v", name, err)
		}
		if adapter.Name() != name {
			t.Fatalf("adapter for %q reports Name() = %q", name, adapter.Name())
		}
	}
}

// TestBuiltinRegistryCapabilitiesMatchAdapters proves the registry's declared
// capabilities per runtime equal the adapter's own Capabilities() — the registry
// is a faithful mirror of the hardcoded values it extracts, so seeding it changes
// nothing.
func TestBuiltinRegistryCapabilitiesMatchAdapters(t *testing.T) {
	reg := BuiltinRuntimeRegistry()
	for _, name := range reg.dispatchableNames() {
		adapter, err := (Factory{}).Adapter(name)
		if err != nil {
			t.Fatalf("Factory.Adapter(%q) error: %v", name, err)
		}
		caps, err := adapter.Capabilities(nil)
		if err != nil {
			t.Fatalf("Capabilities(%q) error: %v", name, err)
		}
		meta, ok := reg.Metadata(name)
		if !ok {
			t.Fatalf("registry missing %q", name)
		}
		if !reflect.DeepEqual(meta.Capabilities, caps) {
			t.Fatalf("%q registry caps %v != adapter caps %v", name, meta.Capabilities, caps)
		}
		if !meta.Dispatchable {
			t.Fatalf("%q should be dispatchable", name)
		}
		if strings.TrimSpace(meta.DefaultModel) != "" {
			t.Fatalf("%q built-in DefaultModel must be empty (CLI default), got %q", name, meta.DefaultModel)
		}
		if strings.TrimSpace(meta.DefaultEffort) != "" {
			t.Fatalf("%q built-in DefaultEffort must be empty (CLI default), got %q", name, meta.DefaultEffort)
		}
		if len(meta.Models) != 0 {
			t.Fatalf("%q built-in Models must be empty (unrestricted), got %v", name, meta.Models)
		}
	}
}

// TestApplyOverridesNoOp proves an empty override set (the default path) returns a
// registry equal to the built-in one — byte-identical default behavior.
func TestApplyOverridesNoOp(t *testing.T) {
	base := BuiltinRuntimeRegistry()
	got, err := base.ApplyOverrides(nil)
	if err != nil {
		t.Fatalf("ApplyOverrides(nil) error: %v", err)
	}
	if !reflect.DeepEqual(got.All(), base.All()) {
		t.Fatalf("no-op ApplyOverrides changed the registry")
	}
}

// TestApplyOverridesPartial proves only the set fields are applied; unset fields
// keep the built-in value.
func TestApplyOverridesPartial(t *testing.T) {
	base := BuiltinRuntimeRegistry()
	got, err := base.ApplyOverrides([]MetadataOverride{{
		Name:             CodexRuntime,
		DefaultModel:     "gpt-5.5-codex",
		DefaultModelSet:  true,
		DefaultEffort:    "high",
		DefaultEffortSet: true,
		Models:           []string{"gpt-5.5-codex", "gpt-5.4-codex"},
		ModelsSet:        true,
	}})
	if err != nil {
		t.Fatalf("ApplyOverrides error: %v", err)
	}
	codex, _ := got.Metadata(CodexRuntime)
	if codex.DefaultModel != "gpt-5.5-codex" {
		t.Fatalf("DefaultModel = %q", codex.DefaultModel)
	}
	if codex.DefaultEffort != "high" {
		t.Fatalf("DefaultEffort = %q", codex.DefaultEffort)
	}
	if !reflect.DeepEqual(codex.Models, []string{"gpt-5.5-codex", "gpt-5.4-codex"}) {
		t.Fatalf("Models = %v", codex.Models)
	}
	// Capabilities were NOT overridden — must keep the built-in value.
	if !reflect.DeepEqual(codex.Capabilities, []string{"review", "implement", "ask"}) {
		t.Fatalf("Capabilities changed unexpectedly: %v", codex.Capabilities)
	}
	// The built-in registry must be untouched (immutability).
	baseCodex, _ := base.Metadata(CodexRuntime)
	if baseCodex.DefaultModel != "" {
		t.Fatalf("ApplyOverrides mutated the source registry: %q", baseCodex.DefaultModel)
	}
	if baseCodex.DefaultEffort != "" {
		t.Fatalf("ApplyOverrides mutated the source registry effort: %q", baseCodex.DefaultEffort)
	}
}

// TestApplyOverridesUnknownRuntime proves config cannot add a new dispatchable
// runtime: an override for an unknown name is a hard error that lists the built-ins.
func TestApplyOverridesUnknownRuntime(t *testing.T) {
	_, err := BuiltinRuntimeRegistry().ApplyOverrides([]MetadataOverride{{Name: "gpt6", DefaultModel: "x", DefaultModelSet: true}})
	if err == nil {
		t.Fatal("ApplyOverrides accepted an unknown runtime")
	}
	for _, name := range SupportedRuntimes() {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error %q must enumerate built-in %q", err.Error(), name)
		}
	}
}

// TestApplyOverridesRejectsBadCapability proves a capability override outside the
// closed set (review/implement/ask) is rejected — a typo cannot silently disable a
// capability.
func TestApplyOverridesRejectsBadCapability(t *testing.T) {
	_, err := BuiltinRuntimeRegistry().ApplyOverrides([]MetadataOverride{{
		Name:            ClaudeRuntime,
		Capabilities:    []string{"review", "deploy"},
		CapabilitiesSet: true,
	}})
	if err == nil {
		t.Fatal("ApplyOverrides accepted an unknown capability")
	}
	if !strings.Contains(err.Error(), "deploy") {
		t.Fatalf("error should name the bad capability: %v", err)
	}
}

// TestMetadataAccessorsAreCopies proves callers cannot mutate the registry through
// a returned metadata value's slices.
func TestMetadataAccessorsAreCopies(t *testing.T) {
	reg := BuiltinRuntimeRegistry()
	meta, _ := reg.Metadata(CodexRuntime)
	meta.Capabilities[0] = "hacked"
	again, _ := reg.Metadata(CodexRuntime)
	if again.Capabilities[0] != "review" {
		t.Fatalf("Metadata returned a shared slice; registry mutated to %v", again.Capabilities)
	}
}
