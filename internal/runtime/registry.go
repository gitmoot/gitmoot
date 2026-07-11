package runtime

import (
	"fmt"
	"sort"
	"strings"
)

// RuntimeMetadata is the declarative, per-runtime metadata that Gitmoot has
// historically hardcoded across the adapter switch and each adapter's methods:
// which capabilities a runtime advertises, its default model/effort, an ADVISORY
// list of known-valid model ids, and a human-readable descriptor of where the
// adapter reads token usage from the CLI's structured output.
//
// The adapter *behavior* — auth/token handling, sandbox policy, session resume,
// stream parsing, transient-retry — stays in Go and is unaffected by this type.
// The registry is a single source of truth consulted for runtime enumeration
// (SupportedRuntimes) and surfaced by `gitmoot runtime list`. Two fields are
// behavioral: DefaultModel is consulted at delivery as the model fallback when
// neither the agent nor the job pins a --model (#652), and DefaultEffort is the
// analogous reasoning-effort fallback. Every other field is inspection-only, so
// seeding it (from the built-in defaults below or from
// operator config) is byte-identical at runtime. In particular Models is advisory:
// Gitmoot never REJECTS a --model based on it, so populating it cannot change how a
// job is delivered.
type RuntimeMetadata struct {
	// Name is the runtime id (codex, claude, kimi, kimi-cli, shell).
	Name string
	// Dispatchable is true for a runtime backed by a compiled Go adapter (every
	// built-in). Config can TWEAK a built-in runtime's metadata but cannot add a
	// new dispatchable runtime — a genuinely new first-class runtime needs a code
	// change (its behavior is code; only its metadata is data). This field exists
	// so that invariant is explicit and testable rather than implied.
	Dispatchable bool
	// Capabilities are the job actions the runtime's adapter advertises. Every
	// Codex, Claude, and Kimi advertise produce. Claude/Kimi dispatch remains
	// fail-closed unless Gitmoot's native Landlock launch sandbox probes healthy.
	Capabilities []string
	// DefaultModel is the runtime's configured default model, surfaced by
	// `gitmoot runtime list` AND consulted at delivery (#652): when NEITHER the
	// agent NOR the job pins a --model, a delivered job falls back to this value as
	// the model passed to the runtime (resolution order: agent/job --model win, then
	// this registry default, then the runtime CLI's own default). Models and
	// Capabilities stay advisory.
	// Empty (the built-in default for every runtime) means "none recorded": nothing
	// is forced, so delivery is byte-identical to before #652.
	DefaultModel string
	// DefaultEffort is the configured Codex reasoning effort consulted at delivery
	// when neither the agent nor the job pins --effort. Other runtimes retain the
	// value as metadata but do not emit a reasoning-effort argument.
	DefaultEffort string
	// Models is an ADVISORY list of known-valid model ids for the runtime. Empty
	// (the default for every built-in) means "unrestricted": Gitmoot passes any
	// --model through unchanged, exactly as today. It is informational only —
	// surfaced by `gitmoot runtime list` so operators have a single place to record
	// which models a runtime accepts — and is never used to reject a model.
	Models []string
	// UsageSource is a human-readable descriptor of where the adapter reads token
	// usage from the runtime CLI's structured output. It documents (and lets a
	// `runtime list` view expose) the per-runtime usage-capture story; it is never
	// consulted to parse anything.
	UsageSource string
	// Description is a one-line summary of the runtime for the `runtime list` view.
	Description string
}

// clone returns a deep copy so a caller can never mutate the built-in registry
// through a returned metadata value's slices.
func (m RuntimeMetadata) clone() RuntimeMetadata {
	out := m
	out.Capabilities = append([]string(nil), m.Capabilities...)
	out.Models = append([]string(nil), m.Models...)
	return out
}

// Registry is an ordered, name-keyed set of RuntimeMetadata. The built-in
// registry (BuiltinRuntimeRegistry) reproduces today's hardcoded behavior; an
// operator-overridden registry is produced by ApplyOverrides. It is immutable
// once built — every accessor returns copies.
type Registry struct {
	order   []string
	entries map[string]RuntimeMetadata
}

// Metadata returns the metadata for a runtime name and whether it is present.
func (r Registry) Metadata(name string) (RuntimeMetadata, bool) {
	m, ok := r.entries[strings.TrimSpace(name)]
	if !ok {
		return RuntimeMetadata{}, false
	}
	return m.clone(), true
}

// Names returns the runtime names in registration order.
func (r Registry) Names() []string {
	return append([]string(nil), r.order...)
}

// All returns every runtime's metadata in registration order.
func (r Registry) All() []RuntimeMetadata {
	out := make([]RuntimeMetadata, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.entries[name].clone())
	}
	return out
}

// dispatchableNames returns the names of the compiled (dispatchable) runtimes in
// registration order. SupportedRuntimes is built from this so the enumeration and
// the adapter Factory share one source of truth.
func (r Registry) dispatchableNames() []string {
	out := make([]string, 0, len(r.order))
	for _, name := range r.order {
		if r.entries[name].Dispatchable {
			out = append(out, name)
		}
	}
	return out
}

// builtinRegistry is the single package-level built-in registry. It is treated as
// immutable; every accessor returns copies, so callers cannot mutate it.
var builtinRegistry = newBuiltinRegistry()

// newBuiltinRegistry seeds the registry from the values Gitmoot hardcodes today,
// so a fresh install with no [runtimes.*] config behaves byte-identically. The
// registration order matches the historical SupportedRuntimes order.
func newBuiltinRegistry() Registry {
	metas := []RuntimeMetadata{
		{
			Name:         CodexRuntime,
			Dispatchable: true,
			Capabilities: []string{"review", "implement", "ask", "produce"},
			UsageSource:  "codex `exec --json` turn.completed usage (session-cumulative on a resumed thread)",
			Description:  "OpenAI Codex CLI (exec/resume, sandbox policy, session index)",
		},
		{
			Name:         ClaudeRuntime,
			Dispatchable: true,
			Capabilities: []string{"review", "implement", "ask", "produce"},
			UsageSource:  "claude `--output-format json` result envelope usage object",
			Description:  "Anthropic Claude Code CLI (session-id/resume, permission modes, transient-retry)",
		},
		{
			Name:         KimiRuntime,
			Dispatchable: true,
			Capabilities: []string{"review", "implement", "ask", "produce"},
			UsageSource:  "kimi `--output-format stream-json` usage event (kimi-code 0.19.2 emits none -> 0)",
			Description:  "Kimi Code CLI (prompt mode, stream-json, per-job fresh session)",
		},
		{
			Name:         KimiCLIRuntime,
			Dispatchable: true,
			Capabilities: []string{"review", "implement", "ask"},
			UsageSource:  "kimi `--print` `--output-format stream-json` usage event (legacy kimi-cli)",
			Description:  "Legacy kimi-cli runtime (--print prompt mode; same model family as kimi)",
		},
		{
			Name:         ShellRuntime,
			Dispatchable: true,
			Capabilities: []string{"review", "implement", "ask"},
			UsageSource:  "none (the shell runtime reports no token usage)",
			Description:  "Subscribe-only shell command runtime (no LLM; drives no-LLM E2Es)",
		},
	}
	reg := Registry{entries: make(map[string]RuntimeMetadata, len(metas))}
	for _, m := range metas {
		reg.order = append(reg.order, m.Name)
		reg.entries[m.Name] = m
	}
	return reg
}

// BuiltinRuntimeRegistry returns the built-in runtime metadata registry seeded
// from Gitmoot's compiled defaults. It is a copy — callers cannot mutate the
// package-level registry through it.
func BuiltinRuntimeRegistry() Registry {
	out := Registry{
		order:   append([]string(nil), builtinRegistry.order...),
		entries: make(map[string]RuntimeMetadata, len(builtinRegistry.entries)),
	}
	for name, m := range builtinRegistry.entries {
		out.entries[name] = m.clone()
	}
	return out
}

// MetadataOverride is a partial, config-sourced patch to a built-in runtime's
// metadata. Only the fields whose companion *Set flag is true (or non-nil
// pointer) are applied; every other key is left at the built-in value. This is
// what makes an override touch EXACTLY the keys an operator wrote and keeps an
// absent [runtimes.*] section byte-identical.
type MetadataOverride struct {
	Name string
	// DefaultModel overrides the runtime's default model when DefaultModelSet.
	DefaultModel    string
	DefaultModelSet bool
	// DefaultEffort overrides the runtime's default reasoning effort when
	// DefaultEffortSet.
	DefaultEffort    string
	DefaultEffortSet bool
	// Models replaces the advisory model list when ModelsSet.
	Models    []string
	ModelsSet bool
	// Capabilities replaces the advertised capabilities when CapabilitiesSet.
	Capabilities    []string
	CapabilitiesSet bool
	// UsageSource overrides the usage descriptor when UsageSourceSet.
	UsageSource    string
	UsageSourceSet bool
}

// ApplyOverrides returns a new Registry with the given operator overrides applied
// on top of the receiver. An override MUST target an existing (built-in) runtime:
// config can retune a runtime's metadata but cannot introduce a new dispatchable
// runtime, which preserves the single-static-binary moat (a new first-class
// runtime needs a code change). An override for an unknown name is a hard error
// so a typo is caught rather than silently ignored. With no overrides the result
// equals the receiver, so the default path stays byte-identical.
func (r Registry) ApplyOverrides(overrides []MetadataOverride) (Registry, error) {
	out := Registry{
		order:   append([]string(nil), r.order...),
		entries: make(map[string]RuntimeMetadata, len(r.entries)),
	}
	for name, m := range r.entries {
		out.entries[name] = m.clone()
	}
	for _, override := range overrides {
		name := strings.TrimSpace(override.Name)
		meta, ok := out.entries[name]
		if !ok {
			return Registry{}, fmt.Errorf("unknown runtime %q in [runtimes.%s]: config can tweak built-in runtime metadata (%s) but cannot add a new runtime; a new first-class runtime requires a code change",
				name, name, strings.Join(out.dispatchableNames(), ", "))
		}
		if override.DefaultModelSet {
			meta.DefaultModel = strings.TrimSpace(override.DefaultModel)
		}
		if override.DefaultEffortSet {
			meta.DefaultEffort = strings.TrimSpace(override.DefaultEffort)
		}
		if override.ModelsSet {
			meta.Models = append([]string(nil), override.Models...)
		}
		if override.CapabilitiesSet {
			if err := validateRuntimeCapabilities(override.Capabilities); err != nil {
				return Registry{}, fmt.Errorf("[runtimes.%s].capabilities: %w", name, err)
			}
			meta.Capabilities = append([]string(nil), override.Capabilities...)
		}
		if override.UsageSourceSet {
			meta.UsageSource = strings.TrimSpace(override.UsageSource)
		}
		out.entries[name] = meta
	}
	return out, nil
}

// runtimeCapabilities is the closed set of job actions a runtime may advertise.
// It matches the actions the dispatch layer understands; a config override that
// names anything else is rejected so a typo cannot silently disable a capability.
func runtimeCapabilities() []string { return []string{"review", "implement", "ask", "produce"} }

func validateRuntimeCapabilities(capabilities []string) error {
	allowed := map[string]bool{}
	for _, c := range runtimeCapabilities() {
		allowed[c] = true
	}
	for _, c := range capabilities {
		if !allowed[strings.TrimSpace(c)] {
			valid := append([]string(nil), runtimeCapabilities()...)
			sort.Strings(valid)
			return fmt.Errorf("unsupported capability %q; use %s", strings.TrimSpace(c), strings.Join(valid, ", "))
		}
	}
	return nil
}
