package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jerryfane/gitmoot/internal/config"
	"github.com/jerryfane/gitmoot/internal/runtime"
)

func writeCLIConfig(t *testing.T, body string) config.Paths {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{ConfigFile: filepath.Join(dir, "config.toml")}
	if err := os.WriteFile(paths.ConfigFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return paths
}

// TestResolveRuntimeRegistryDefaultByteIdentical proves that with no [runtimes.*]
// section the resolved registry equals the built-in one.
func TestResolveRuntimeRegistryDefaultByteIdentical(t *testing.T) {
	paths := writeCLIConfig(t, "[paths]\ndatabase = \"x\"\n")
	got, err := resolveRuntimeRegistry(paths)
	if err != nil {
		t.Fatalf("resolveRuntimeRegistry error: %v", err)
	}
	if !reflect.DeepEqual(got.All(), runtime.BuiltinRuntimeRegistry().All()) {
		t.Fatalf("resolved registry differs from built-in with no overrides")
	}
}

// TestResolveRuntimeRegistryMissingFile proves a missing config file resolves to
// the built-in registry rather than erroring.
func TestResolveRuntimeRegistryMissingFile(t *testing.T) {
	paths := config.Paths{ConfigFile: filepath.Join(t.TempDir(), "nope.toml")}
	got, err := resolveRuntimeRegistry(paths)
	if err != nil {
		t.Fatalf("resolveRuntimeRegistry error: %v", err)
	}
	if !reflect.DeepEqual(got.All(), runtime.BuiltinRuntimeRegistry().All()) {
		t.Fatalf("missing file should resolve to built-in registry")
	}
}

// TestResolveRuntimeRegistryOverride proves config overrides are applied on top of
// the built-ins.
func TestResolveRuntimeRegistryOverride(t *testing.T) {
	paths := writeCLIConfig(t, `
[runtimes.codex]
default_model = "gpt-5.5-codex"
default_effort = "high"
models = ["gpt-5.5-codex"]
`)
	got, err := resolveRuntimeRegistry(paths)
	if err != nil {
		t.Fatalf("resolveRuntimeRegistry error: %v", err)
	}
	codex, ok := got.Metadata(runtime.CodexRuntime)
	if !ok {
		t.Fatal("codex missing")
	}
	if codex.DefaultModel != "gpt-5.5-codex" {
		t.Fatalf("DefaultModel = %q", codex.DefaultModel)
	}
	if codex.DefaultEffort != "high" {
		t.Fatalf("DefaultEffort = %q", codex.DefaultEffort)
	}
	if !reflect.DeepEqual(codex.Models, []string{"gpt-5.5-codex"}) {
		t.Fatalf("Models = %v", codex.Models)
	}
}

// TestResolveRuntimeRegistryUnknownErrors proves an unknown runtime name in config
// surfaces the moat-preserving error.
func TestResolveRuntimeRegistryUnknownErrors(t *testing.T) {
	paths := writeCLIConfig(t, "[runtimes.gpt6]\ndefault_model = \"x\"\n")
	if _, err := resolveRuntimeRegistry(paths); err == nil {
		t.Fatal("expected error for unknown runtime")
	}
}

// TestRunRuntimeListText covers the human table output on the default registry.
func TestRunRuntimeListText(t *testing.T) {
	paths := writeCLIConfig(t, "[paths]\ndatabase = \"x\"\n")
	var stdout, stderr bytes.Buffer
	code := runRuntimeList([]string{"--home", homeFromConfig(t, paths)}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, name := range runtime.SupportedRuntimes() {
		if !strings.Contains(out, name) {
			t.Fatalf("table missing runtime %q:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "review,implement,ask") {
		t.Fatalf("table missing capabilities:\n%s", out)
	}
}

// TestRunRuntimeListJSON covers the JSON output shape.
func TestRunRuntimeListJSON(t *testing.T) {
	paths := writeCLIConfig(t, "[runtimes.codex]\ndefault_effort = \"high\"\n")
	var stdout, stderr bytes.Buffer
	code := runRuntimeList([]string{"--home", homeFromConfig(t, paths), "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d stderr=%s", code, stderr.String())
	}
	var entries []runtimeListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if len(entries) != len(runtime.SupportedRuntimes()) {
		t.Fatalf("expected %d entries, got %d", len(runtime.SupportedRuntimes()), len(entries))
	}
	if entries[0].Name != runtime.CodexRuntime || !entries[0].Dispatchable {
		t.Fatalf("first entry wrong: %+v", entries[0])
	}
	if entries[0].DefaultEffort != "high" {
		t.Fatalf("codex default_effort = %q, want high", entries[0].DefaultEffort)
	}
}

// TestRunRuntimeListUnknownRuntimeExits proves the command exits non-zero on a bad
// override.
func TestRunRuntimeListUnknownRuntimeExits(t *testing.T) {
	paths := writeCLIConfig(t, "[runtimes.gpt6]\ndefault_model = \"x\"\n")
	var stdout, stderr bytes.Buffer
	code := runRuntimeList([]string{"--home", homeFromConfig(t, paths)}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit, stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gpt6") {
		t.Fatalf("stderr should mention the bad runtime: %s", stderr.String())
	}
}

// TestRuntimeDefaultModelResolverReadsConfig proves the HOME-AWARE delivery hook
// (#652) returns the configured [runtimes.<rt>].default_model for a runtime, and
// resolves NOTHING (byte-identical) for a runtime with no override.
func TestRuntimeDefaultModelResolverReadsConfig(t *testing.T) {
	src := writeCLIConfig(t, "[runtimes.codex]\ndefault_model = \"gpt-5.5\"\n")
	home := homeFromConfig(t, src)
	resolve := runtimeDefaultModelResolver(home)
	if got := resolve(runtime.CodexRuntime); got != "gpt-5.5" {
		t.Fatalf("resolve(codex) = %q, want gpt-5.5", got)
	}
	// A runtime with no override records no default: the hook forces nothing.
	if got := resolve(runtime.ClaudeRuntime); got != "" {
		t.Fatalf("resolve(claude) = %q, want empty (no override)", got)
	}
}

func TestRuntimeDefaultEffortResolverReadsConfig(t *testing.T) {
	src := writeCLIConfig(t, "[runtimes.codex]\ndefault_effort = \"high\"\n")
	home := homeFromConfig(t, src)
	resolve := runtimeDefaultEffortResolver(home)
	if got := resolve(runtime.CodexRuntime); got != "high" {
		t.Fatalf("resolve(codex) = %q, want high", got)
	}
	if got := resolve(runtime.ClaudeRuntime); got != "" {
		t.Fatalf("resolve(claude) = %q, want empty (no override)", got)
	}
}

func TestRuntimeDefaultEffortResolverAbsentConfig(t *testing.T) {
	if got := runtimeDefaultEffortResolver(t.TempDir())(runtime.CodexRuntime); got != "" {
		t.Fatalf("missing-config resolve(codex) = %q, want empty", got)
	}
	if got := runtimeDefaultEffortResolver("")(runtime.CodexRuntime); got != "" {
		t.Fatalf("empty-home resolve(codex) = %q, want empty", got)
	}
}

// TestRuntimeDefaultModelResolverAbsentConfigByteIdentical proves the fail-open
// contract: with no config file (fresh box), an empty home, or an unknown runtime,
// the hook resolves "" so delivery forces no model — byte-identical to before #652.
func TestRuntimeDefaultModelResolverAbsentConfigByteIdentical(t *testing.T) {
	missingHome := t.TempDir() // no config.toml written anywhere under it
	if got := runtimeDefaultModelResolver(missingHome)(runtime.CodexRuntime); got != "" {
		t.Fatalf("missing-config resolve(codex) = %q, want empty", got)
	}
	if got := runtimeDefaultModelResolver("")(runtime.CodexRuntime); got != "" {
		t.Fatalf("empty-home resolve(codex) = %q, want empty", got)
	}
	src := writeCLIConfig(t, "[runtimes.codex]\ndefault_model = \"gpt-5.5\"\n")
	if got := runtimeDefaultModelResolver(homeFromConfig(t, src))("nonexistent-runtime"); got != "" {
		t.Fatalf("unknown-runtime resolve = %q, want empty", got)
	}
}

// TestRuntimeDefaultModelResolverSkipsBadSection proves the DELIVERY path is
// PER-SECTION resilient (#652 LOW): a config with one VALID [runtimes.codex]
// default_model plus one bad section (an unknown-runtime typo) still resolves the
// codex override — a single bad section is skipped, not a whole-config failure that
// silently drops every valid override at delivery.
func TestRuntimeDefaultModelResolverSkipsBadSection(t *testing.T) {
	src := writeCLIConfig(t, `
[runtimes.codex]
default_model = "gpt-5.5"

[runtimes.codxe]
default_model = "typo-runtime"
`)
	home := homeFromConfig(t, src)
	resolve := runtimeDefaultModelResolver(home)
	// The valid codex override still takes effect despite the bad sibling section.
	if got := resolve(runtime.CodexRuntime); got != "gpt-5.5" {
		t.Fatalf("resolve(codex) = %q, want gpt-5.5 (bad sibling section must not drop it)", got)
	}
	// The bad section is skipped entirely, not surfaced as a phantom runtime.
	if got := resolve("codxe"); got != "" {
		t.Fatalf("resolve(codxe) = %q, want empty (bad section skipped)", got)
	}
}

// TestRuntimeDefaultModelResolverSkipsBadCapabilitySection proves the same
// per-section resilience for an invalid-capability section: the whole-config apply
// would reject it, but the delivery resolver skips only that section and keeps a
// valid sibling's default_model.
func TestRuntimeDefaultModelResolverSkipsBadCapabilitySection(t *testing.T) {
	src := writeCLIConfig(t, `
[runtimes.codex]
default_model = "gpt-5.5"

[runtimes.claude]
capabilities = ["review", "bogus"]
`)
	home := homeFromConfig(t, src)
	resolve := runtimeDefaultModelResolver(home)
	if got := resolve(runtime.CodexRuntime); got != "gpt-5.5" {
		t.Fatalf("resolve(codex) = %q, want gpt-5.5 (bad capability sibling must not drop it)", got)
	}
	// The claude section is skipped, so it records no default (byte-identical).
	if got := resolve(runtime.ClaudeRuntime); got != "" {
		t.Fatalf("resolve(claude) = %q, want empty (bad capability section skipped)", got)
	}
}

// homeFromConfig writes the given config body into a home layout so a --home flag
// resolves ConfigFile to it. The CLI resolves --home via config.PathsForHome, so
// place the file at <home>/.gitmoot/config.toml.
func homeFromConfig(t *testing.T, src config.Paths) string {
	t.Helper()
	home := t.TempDir()
	paths := config.PathsForHome(home)
	if err := os.MkdirAll(filepath.Dir(paths.ConfigFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body, err := os.ReadFile(src.ConfigFile)
	if err != nil {
		t.Fatalf("read src: %v", err)
	}
	if err := os.WriteFile(paths.ConfigFile, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return home
}
